//go:build !arctest

package server

import "time"

// confirmWait is how long the sender waits for the ok of a receiver.
func confirmWait(string) time.Duration { return 30 * time.Second }

// beforePutFinish runs before the app checks the bytes of a put; a test
// build waits there, as a slow disk does.
func beforePutFinish(string) {}
