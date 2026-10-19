//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

// The requests that read and set the mode of a terminal.
const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
)
