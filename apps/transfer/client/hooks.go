//go:build !arctest

package client

import "time"

// afterOK runs after the receiver sends ok; a test build waits there.
func afterOK() {}

// dropOK says whether the receiver skips its ok; a test build can, to act
// as an ok that the network lost.
func dropOK() bool { return false }

// putConfirmWait is how long a put waits for the ok of the app: 30 s, and
// 1 s more for each 4 MiB that it gave, because the app puts the file on
// its disk before it says ok, and a slow disk takes that long.
func putConfirmWait(size int64) time.Duration {
	return confirmWait(30*time.Second, 4<<20, size)
}

// beforeFinish runs when end arrives, before the check of the bytes; a test
// build waits there, as a slow disk does.
func beforeFinish() {}
