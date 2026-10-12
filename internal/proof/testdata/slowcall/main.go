// Command slowcall runs a provider program, and holds each line to it for
// SLOWCALL_MS milliseconds. A proof uses it as a slow link to a storage
// citizen: each ARC call to the program then takes that much longer.
// SLOWCALL_PROGRAM names the program. With SLOWCALL_LOG, it appends the
// start and the end of each hold to that file, in Unix nanoseconds.
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
	var holds *os.File
	if path := os.Getenv("SLOWCALL_LOG"); path != "" {
		if holds, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "slowcall:", err)
			os.Exit(2)
		}
	}
	go func() {
		lines := bufio.NewReader(os.Stdin)
		for {
			line, err := lines.ReadBytes('\n')
			if len(line) > 0 {
				start := time.Now()
				time.Sleep(time.Duration(ms) * time.Millisecond)
				if holds != nil {
					fmt.Fprintf(holds, "%d %d\n", start.UnixNano(), time.Now().UnixNano())
				}
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
