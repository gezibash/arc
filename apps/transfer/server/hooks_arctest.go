//go:build arctest

package server

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// confirmWait is how long the sender waits for the ok of a receiver. In a
// build with the tag arctest, the file arctest-confirm-wait in the state
// directory can hold a shorter time, so that one test lets the sender give
// up and the others keep 30 s.
func confirmWait(state string) time.Duration {
	raw, err := os.ReadFile(filepath.Join(state, "arctest-confirm-wait"))
	if err == nil {
		if d, err := time.ParseDuration(strings.TrimSpace(string(raw))); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// beforePutFinish, in a build with the tag arctest, runs before the app
// checks the bytes of a put. It waits the time in the file
// arctest-put-finish-delay of the state directory, as a slow disk does.
// With the file arctest-put-gone there, it returns true, and the app ends
// the connection with no answer, as an app that is gone does.
func beforePutFinish(state string) bool {
	if _, err := os.Stat(filepath.Join(state, "arctest-put-gone")); err == nil {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(state, "arctest-put-finish-delay"))
	if err != nil {
		return false
	}
	if d, err := time.ParseDuration(strings.TrimSpace(string(raw))); err == nil && d > 0 {
		time.Sleep(d)
	}
	return false
}
