//go:build arctest

package client

import (
	"os"
	"time"
)

// afterOK waits TRANSFER_TEST_OK_DELAY after the receiver sends ok, in a
// build with the tag arctest, so that a test can let the sender close the
// connection before the receiver goes on. A release build does not wait.
func afterOK() {
	if d, err := time.ParseDuration(os.Getenv("TRANSFER_TEST_OK_DELAY")); err == nil {
		time.Sleep(d)
	}
}
