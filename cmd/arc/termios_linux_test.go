package main

import "golang.org/x/sys/unix"

// The requests that read and set the mode of a terminal.
const (
	getTermios = unix.TCGETS
	setTermios = unix.TCSETS
)
