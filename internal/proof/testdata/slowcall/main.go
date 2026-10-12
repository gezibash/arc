// Command slowcall runs a provider program, and holds each line to it for
// SLOWCALL_MS milliseconds. A proof uses it as a slow link to a storage
// citizen: each ARC call to the program then takes that much longer.
// SLOWCALL_PROGRAM names the program.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func main() {
	ms, err := strconv.Atoi(os.Getenv("SLOWCALL_MS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "slowcall: SLOWCALL_MS:", err)
		os.Exit(2)
	}
	command := exec.Command(os.Getenv("SLOWCALL_PROGRAM"), os.Args[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	in, err := command.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "slowcall:", err)
		os.Exit(2)
	}
	if err := command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "slowcall:", err)
		os.Exit(2)
	}
	go func() {
		lines := bufio.NewReader(os.Stdin)
		for {
			line, err := lines.ReadBytes('\n')
			if len(line) > 0 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
				if _, werr := in.Write(line); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = in.Close()
	}()
	if err := command.Wait(); err != nil {
		os.Exit(1)
	}
}
