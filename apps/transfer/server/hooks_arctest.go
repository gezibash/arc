//go:build arctest

package server

import (
	"os"
	"time"
)

// confirmWait is how long the sender waits for the ok of a receiver:
// TRANSFER_TEST_CONFIRM_WAIT in a build with the tag arctest, so that a
// test can let the sender give up, and 30 s otherwise.
func confirmWait() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TRANSFER_TEST_CONFIRM_WAIT")); err == nil {
		return d
	}
	return 30 * time.Second
}
