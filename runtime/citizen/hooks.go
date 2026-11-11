//go:build !arctest

package citizen

import (
	"io"
	"time"
)

// pauseAfterSave does nothing in a release build. A build with the tag
// arctest can stop a write of self data between two events.
func pauseAfterSave(int, io.Writer) {}

// testNow is nil in a release build: a sync uses the clock.
func testNow() func() time.Time { return nil }
