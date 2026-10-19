package client

import (
	"testing"
	"time"
)

// A put waits 30 s and 1 s more for each 4 MiB, at most 6 h more, also for
// sizes that would overflow a product with time.Second.
func TestThePutWaitGrowsWithTheSize(t *testing.T) {
	for _, c := range []struct {
		size int64
		want time.Duration
	}{{0, 30 * time.Second}, {40 << 20, 40 * time.Second}, {10 << 30, 30*time.Second + 2560*time.Second}, {1 << 40, 30*time.Second + 6*time.Hour}, {1<<63 - 1, 30*time.Second + 6*time.Hour}, {-1, 30 * time.Second}} {
		if got := putConfirmWait(c.size); got != c.want {
			t.Errorf("putConfirmWait(%d) = %v, want %v", c.size, got, c.want)
		}
	}
}
