package client

import (
	"math"
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

// The edges of the put wait, from Cato: a whole number of 4 MiB, one byte
// either side, the size where a product with time.Second would overflow,
// and the 6 h cap. Odd rates of confirmWait give the base.
func TestThePutWaitAtItsEdges(t *testing.T) {
	const r = 4 << 20
	frac := func(n int64) time.Duration {
		return time.Duration(float64(n%r) / float64(r) * float64(time.Second))
	}
	capEdge := int64(21600) * r
	for _, c := range []struct {
		size int64
		want time.Duration
	}{
		{0, 30 * time.Second}, {-1, 30 * time.Second}, {math.MinInt64, 30 * time.Second},
		{1, 30*time.Second + frac(1)},
		{r - 1, 30*time.Second + frac(r-1)},
		{r, 31 * time.Second}, {r + 1, 31*time.Second + frac(1)},
		{9223372036, 30*time.Second + 2199*time.Second + frac(9223372036)},
		{9223372037, 30*time.Second + 2199*time.Second + frac(9223372037)},
		{capEdge - 1, 30*time.Second + 21599*time.Second + frac(r-1)},
		{capEdge, 30*time.Second + 6*time.Hour}, {capEdge + 1, 30*time.Second + 6*time.Hour},
		{math.MaxInt64, 30*time.Second + 6*time.Hour},
	} {
		if got := putConfirmWait(c.size); got != c.want {
			t.Errorf("putConfirmWait(%d) = %v, want %v", c.size, got, c.want)
		}
	}
	for _, rate := range []int64{0, -1} {
		if got := confirmWait(time.Second, rate, 100); got != time.Second {
			t.Errorf("confirmWait with rate %d = %v, want the base", rate, got)
		}
	}
	if got := confirmWait(time.Second, 1, math.MaxInt64); got != time.Second+6*time.Hour {
		t.Errorf("confirmWait with rate 1 and the largest size = %v, want the cap", got)
	}
}
