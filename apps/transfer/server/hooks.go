//go:build !arctest

package server

import "time"

// confirmWait is how long the sender waits for the ok of a receiver.
func confirmWait() time.Duration { return 30 * time.Second }
