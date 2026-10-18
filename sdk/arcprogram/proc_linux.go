package arcprogram

import (
	"os/exec"
	"syscall"
)

// prepare makes Linux kill arc when the thread that started it ends, so a
// client program that is killed leaves no arc behind. The caller holds its
// thread while it starts arc.
func prepare(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
