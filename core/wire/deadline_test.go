package wire_test

import (
	"context"
	"testing"
	"time"
	"unsafe"

	"github.com/gezibash/arc/core/wire"
)

// slept returns t with its wall reading moved back by d and its monotonic
// reading kept. A deadline that a context set before the machine slept for
// d is in this state after the wake: macOS stops the monotonic clock during
// sleep, and the wall clock goes on. Go cannot make such a time by its API,
// so the test changes the 33 bits of wall seconds that the time package
// keeps beside its monotonic reading.
func slept(t *testing.T, at time.Time, d time.Duration) time.Time {
	t.Helper()
	const hasMonotonic, nsecBits = 1 << 63, 30
	wall := (*uint64)(unsafe.Pointer(&at))
	if *wall&hasMonotonic == 0 {
		t.Fatal("the time has no monotonic reading")
	}
	*wall -= uint64(d/time.Second) << nsecBits
	return at
}

// deadlineCtx is a context with a given deadline.
type deadlineCtx struct {
	context.Context
	deadline time.Time
}

func (c deadlineCtx) Deadline() (time.Time, bool) { return c.deadline, true }

// A caller sends the time that remains of its own budget. A deadline that
// was set before a sleep of ten minutes still has 15 s on the monotonic clock
// of the caller, so the provider must not get a deadline ten minutes in the
// past (wire.Budget of the provider reads the wall clock).
func TestADeadlineSetBeforeASleepStillHasItsTime(t *testing.T) {
	deadline := slept(t, time.Now().Add(15*time.Second), 10*time.Minute)
	if remaining := time.Until(deadline); remaining < 14*time.Second {
		t.Fatalf("the test deadline has %s on the monotonic clock, want about 15 s", remaining)
	}
	sent := time.UnixMilli(wire.Deadline(deadlineCtx{context.Background(), deadline}))
	if left := time.Until(sent); left < 14*time.Second || left > 16*time.Second {
		t.Errorf("the sent deadline is %s from now on the wall clock, want about 15 s", left.Round(time.Second))
	}
}
