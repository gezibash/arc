//go:build !arctest

package client

// afterOK runs after the receiver sends ok; a test build waits there.
func afterOK() {}
