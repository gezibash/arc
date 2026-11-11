package host

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/runtime/catalog"
)

// announcer signs the announcements of arc serve. Each is at least a second
// newer than the newest that the store holds, so that it never ties with an
// older one of the same second (#422). Until a relay answers for the
// announcements that the relays hold, arc cannot know the newest: it signs
// nothing, and says so once.
type announcer struct {
	// known and ahead are read by the watches of other goroutines.
	known, ahead atomic.Bool
	told         bool
	// tried is the wall time of the last call to announce. A machine that
	// sleeps stops the monotonic clock of a ticker, but not the wall clock
	// by which a caller judges an announcement (catalog.Current).
	tried nostr.Timestamp
	// pull asks the relays for the announcements, and is nil when one
	// answered.
	pull func() error
	// at is the time to sign at, after the newest in the store.
	at   func() (nostr.Timestamp, error)
	sign func(at nostr.Timestamp) error
	say  func(text string)
}

func (a *announcer) announce(now nostr.Timestamp) error {
	a.tried = now
	if !a.known.Load() {
		a.known.Store(a.pull() == nil)
	}
	if !a.known.Load() {
		if !a.told {
			a.say("is not announced yet: no relay has answered, so arc cannot know the newest announcement; it asks again")
			a.told = true
		}
		return nil
	}
	at, err := a.at()
	// An announcement far ahead of this clock, from a wrong clock or a
	// hostile relay, holds the new one back, but arc serve keeps serving:
	// to stop would turn it into an outage. It says so once (#445).
	if ahead, ok := errors.AsType[*catalog.AheadError](err); ok {
		if !a.ahead.Load() {
			a.say(fmt.Sprintf("is not announced: an announcement is %s ahead of this clock; arc announces again in %s", ahead.Ahead, ahead.Ahead-catalog.MaxSkew))
		}
		a.ahead.Store(true)
		return nil
	}
	a.ahead.Store(false)
	if err != nil {
		return err
	}
	return a.sign(at)
}

// due says whether catalog.Refresh passed on the wall clock since the last
// call to announce.
func (a *announcer) due(now nostr.Timestamp) bool {
	return now-a.tried >= nostr.Timestamp(catalog.Refresh/time.Second)
}

// waiting says whether the announcer waits: for a relay that answers, or
// for the clock to reach an announcement far ahead of it.
func (a *announcer) waiting() bool { return !a.known.Load() || a.ahead.Load() }

// waitingWriter drops what a logger writes while waiting says so.
type waitingWriter struct {
	to      io.Writer
	waiting func() bool
}

func (w waitingWriter) Write(p []byte) (int, error) {
	if w.waiting() {
		return len(p), nil
	}
	return w.to.Write(p)
}
