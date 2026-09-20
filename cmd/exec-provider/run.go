package main

import (
	"bytes"
	"os/exec"
	"syscall"
	"time"
	"unicode/utf8"
)

// result is what one command left behind.
type result struct {
	Exit      int    `json:"exit"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
}

// start builds the process. A new session puts the command and its children
// in one process group, so a timeout stops the whole tree.
func start(cmd *command) *exec.Cmd {
	process := exec.Command(cmd.Argv[0], cmd.Argv[1:]...)
	process.Dir = cmd.Dir
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return process
}

// runCommand runs one command and waits for it.
func runCommand(cfg *config, cmd *command) (*result, error) {
	process := start(cmd)

	var stdout, stderr bytes.Buffer
	process.Stdin = bytes.NewReader(cmd.Stdin)
	process.Stdout = &stdout
	process.Stderr = &stderr

	if err := process.Start(); err != nil {
		return nil, err
	}

	timedOut := wait(process, time.Duration(cmd.TimeoutMS)*time.Millisecond)

	// Standard output and standard error share one budget.
	limit := cfg.Limits.OutputBytes
	out, outClipped := clip(stdout.Bytes(), limit)
	err, errClipped := clip(stderr.Bytes(), max(0, limit-stdout.Len()))

	return &result{
		Exit:      process.ProcessState.ExitCode(),
		Stdout:    out,
		Stderr:    err,
		TimedOut:  timedOut,
		Truncated: outClipped || errClipped,
	}, nil
}

// wait waits for the process, and stops its whole group at the timeout.
func wait(process *exec.Cmd, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		process.Wait()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return false
	case <-timer.C:
		killGroup(process)
		<-done
		return true
	}
}

// killGroup stops the command and every child that it started.
func killGroup(process *exec.Cmd) {
	if process.Process == nil {
		return
	}
	if err := syscall.Kill(-process.Process.Pid, syscall.SIGKILL); err != nil {
		process.Process.Kill()
	}
}

// clip cuts the output to the budget. The text that is left is valid UTF-8,
// because a JSON string holds no broken bytes.
func clip(data []byte, limit int) (string, bool) {
	if len(data) <= limit {
		return safeText(data), false
	}
	return safeText(data[:limit]), true
}

// safeText replaces every byte that is not UTF-8.
func safeText(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return string(bytes.ToValidUTF8(data, []byte("�")))
}
