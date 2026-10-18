//go:build !linux

package arcprogram

import "os/exec"

// prepare does nothing: only Linux kills a child when its parent dies. On
// another system, an arc whose client program is killed with SIGKILL runs
// on.
func prepare(*exec.Cmd) {}
