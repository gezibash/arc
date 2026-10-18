//go:build unix

package arcprogram

import (
	"os/exec"
	"syscall"
)

// inGroup runs a command in a process group of its own, and kills the
// whole group at the end of its context.
func inGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
}

// interrupt sends an interrupt to the process group of a command.
func interrupt(command *exec.Cmd) error {
	return syscall.Kill(-command.Process.Pid, syscall.SIGINT)
}
