//go:build arctest

package client

import (
	"os"
	"strconv"
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

// dropOK says whether TRANSFER_TEST_DROP_OK is set, in a build with the tag
// arctest: the receiver then skips its ok, as if the network lost it.
func dropOK() bool { return os.Getenv("TRANSFER_TEST_DROP_OK") != "" }

// putConfirmWait is how long a put waits for the ok of the app: as in a
// release build, but TRANSFER_TEST_CONFIRM_BASE and the bytes per second in
// TRANSFER_TEST_CONFIRM_RATE can make it shorter for a test.
func putConfirmWait(size int64) time.Duration {
	base, rate := 30*time.Second, int64(4<<20)
	if d, err := time.ParseDuration(os.Getenv("TRANSFER_TEST_CONFIRM_BASE")); err == nil {
		base = d
	}
	if r, err := strconv.ParseInt(os.Getenv("TRANSFER_TEST_CONFIRM_RATE"), 10, 64); err == nil && r > 0 {
		rate = r
	}
	return confirmWait(base, rate, size)
}

// beforeFinish waits TRANSFER_TEST_FINISH_DELAY when end arrives, before
// the check of the bytes, in a build with the tag arctest, as a slow disk
// does.
func beforeFinish() {
	if d, err := time.ParseDuration(os.Getenv("TRANSFER_TEST_FINISH_DELAY")); err == nil {
		time.Sleep(d)
	}
}
