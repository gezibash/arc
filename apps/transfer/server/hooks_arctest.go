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
		if d, err := time.ParseDuration(strings.TrimSpace(string(raw))); err == nil {
			return d
		}
	}
	return 30 * time.Second
}

// beforePutFinish waits the time in the file arctest-put-finish-delay of the
// state directory, in a build with the tag arctest, before the app checks
// the bytes of a put, as a slow disk does.
func beforePutFinish(state string) {
	raw, err := os.ReadFile(filepath.Join(state, "arctest-put-finish-delay"))
	if err != nil {
		return
	}
	if d, err := time.ParseDuration(strings.TrimSpace(string(raw))); err == nil {
		time.Sleep(d)
	}
}
