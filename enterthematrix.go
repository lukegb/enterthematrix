/*
Copyright 2017 Luke Granger-Brown

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

var (
	validName = regexp.MustCompile(`^.*_[a-f0-9]{8}$`)
)

func selectContainer(cs []types.Container) types.Container {
	if len(cs) == 1 {
		c := cs[0]
		fmt.Printf("Automatically selected %s, as it's the only running server.\n", c.Names[0])
		return c
	}

	fmt.Printf("There are %d running servers:\n", len(cs))
	for n, c := range cs {
		fmt.Printf(" [%d] %s\n", n, c.Names[0])
	}
	fmt.Printf("\n")

	fmt.Printf("Choice: ")
	var i int
	for {
		_, err := fmt.Scanf("%d", &i)
		if err != nil {
			fmt.Printf("Hmm, that doesn't look like a number: %v", err)
			continue
		}

		if i < 0 || i >= len(cs) {
			fmt.Printf("Please enter a number between 0 and %d inclusive.", len(cs)-1)
			continue
		}

		break
	}

	return cs[i]
}

func main() {
	cli, err := client.NewEnvClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to Docker API: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list containers: %v\n", err)
		os.Exit(1)
	}

	var validContainers []types.Container
	for _, container := range containers {
		if len(container.Names) != 1 {
			// Get out of here with your multiple names
			continue
		}

		name := container.Names[0]
		if !validName.MatchString(name) {
			// Nope, this doesn't look like a container we want
			continue
		}

		validContainers = append(validContainers, container)
	}

	if len(validContainers) == 0 {
		fmt.Fprintf(os.Stderr, "Whoops - there are no running servers at the moment. Start one, and come back later.\n")
		os.Exit(1)
	}

	selectedContainer := selectContainer(validContainers)
	// Create exec description
	createResp, err := cli.ContainerExecCreate(ctx, selectedContainer.ID, types.ExecConfig{
		Tty:          true,
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          []string{"/bin/bash"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create an exec environment: %v\n", err)
		os.Exit(1)
	}

	execID := createResp.ID

	// Attach to the exec environment
	hijackResp, err := cli.ContainerExecAttach(ctx, execID, types.ExecConfig{
		Tty:          true,
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to attach to exec environment: %v\n", err)
		os.Exit(1)
	}
	defer hijackResp.Close()
	defer hijackResp.CloseWrite()

	winchChan := make(chan os.Signal)
	signal.Notify(winchChan, syscall.SIGWINCH)
	go func() {
		for range winchChan {
			width, height, err := terminal.GetSize(syscall.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				continue
			}
			if err := cli.ContainerExecResize(ctx, execID, types.ResizeOptions{
				Height: uint(height),
				Width:  uint(width),
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to resize container TTY: %v\n", err)
			}
		}
	}()
	defer close(winchChan)
	time.Sleep(100 * time.Millisecond)
	winchChan <- syscall.SIGWINCH

	// switch to raw
	terminalState, err := terminal.MakeRaw(syscall.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to make terminal raw: %v\n", err)
		os.Exit(1)
	}
	defer terminal.Restore(syscall.Stdin, terminalState)

	go io.Copy(hijackResp.Conn, os.Stdin)
	io.Copy(os.Stdout, hijackResp.Conn)
}
