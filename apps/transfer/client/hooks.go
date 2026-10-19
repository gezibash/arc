//go:build !arctest

package client

// afterOK runs after the receiver sends ok; a test build waits there.
func afterOK() {}

// dropOK says whether the receiver skips its ok; a test build can, to act
// as an ok that the network lost.
func dropOK() bool { return false }
