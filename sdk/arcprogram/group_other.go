//go:build !unix

package arcprogram

import (
	"os"
	"os/exec"
)

// inGroup does nothing on a system with no process groups.
func inGroup(*exec.Cmd) {}

// interrupt sends an interrupt to a command.
func interrupt(command *exec.Cmd) error { return command.Process.Signal(os.Interrupt) }
