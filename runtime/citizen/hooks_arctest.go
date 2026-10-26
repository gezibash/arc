//go:build arctest

package citizen

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// pauseAfterSave stops a write of self data after its saved-th event, in a
// build with the tag arctest, when ARC_TEST_PAUSE_PUBLISH is <n>:<file>.
// It says so on errs, and waits until the file exists. A test of the
// storage spec, section 4.7, runs a repair while a real write waits there.
func pauseAfterSave(saved int, errs io.Writer) {
	n, file, ok := strings.Cut(os.Getenv("ARC_TEST_PAUSE_PUBLISH"), ":")
	if !ok || n != strconv.Itoa(saved) {
		return
	}
	if errs != nil {
		fmt.Fprintln(errs, "arctest: the write waits after", saved, "events")
	}
	for {
		if _, err := os.Stat(file); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// testNow is the clock of a sync, ARC_TEST_NOW_OFFSET from now, in a
// build with the tag arctest, so that a test runs a repair hours later.
func testNow() func() time.Time {
	offset, err := time.ParseDuration(os.Getenv("ARC_TEST_NOW_OFFSET"))
	if err != nil {
		return nil
	}
	return func() time.Time { return time.Now().Add(offset) }
}
