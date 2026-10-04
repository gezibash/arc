package server

import (
	"bytes"
	"context"
	"os/exec"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gezibash/arc/sdk/limitio"
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
func runCommand(ctx context.Context, cfg *config, cmd *command) (*result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	process := start(cmd)

	stdout := limitio.Buffer{Max: cfg.Limits.OutputBytes}
	stderr := limitio.Buffer{Max: cfg.Limits.OutputBytes}
	process.Stdin = bytes.NewReader(cmd.Stdin)
	process.Stdout = &stdout
	process.Stderr = &stderr

	if err := process.Start(); err != nil {
		return nil, err
	}

	timedOut := wait(ctx, process, time.Duration(cmd.TimeoutMS)*time.Millisecond)

	// Standard output and standard error share one budget.
	limit := cfg.Limits.OutputBytes
	out, outClipped := clip(stdout.Bytes(), limit)
	err, errClipped := clip(stderr.Bytes(), max(0, limit-stdout.Len()))

	return &result{
		Exit:      process.ProcessState.ExitCode(),
		Stdout:    out,
		Stderr:    err,
		TimedOut:  timedOut,
		Truncated: outClipped || errClipped || stdout.Truncated || stderr.Truncated,
	}, nil
}

// wait waits for the process, and stops its whole group at the timeout.
func wait(ctx context.Context, process *exec.Cmd, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_ = process.Wait()
		close(done)
	}()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-done:
		return false
	case <-ctx.Done():
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
		_ = process.Process.Kill()
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
