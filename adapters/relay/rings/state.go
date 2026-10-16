package rings

import "time"

// Retries are the waits before the second and the third try of a ring
// that got no 2xx answer (docs/rings/SPEC.md, section 3.6).
var Retries = []time.Duration{time.Minute, 5 * time.Minute}

// Abandon is how long a registration lives with no 2xx answer, after its
// first failed try.
const Abandon = 7 * 24 * time.Hour

// settle is how long a wrap waits before it can ring. khatru saves a wrap
// before it broadcasts it, so an open subscription of the citizen reads the
// wrap only just after the save.
const settle = time.Second

// state is what the relay knows of one registered citizen. Times are Unix
// seconds; zero is never.
type state struct {
	Settings
	// SealedKey is the ring key, encrypted with the secret of the relay.
	SealedKey []byte `json:"sealed_key"`
	// Relay is the wss URL of this relay, as the citizen named it.
	Relay   string `json:"relay"`
	Created int64  `json:"created"`

	// LastRead is the time of the last read.
	LastRead int64 `json:"last_read"`
	// Since is when the first wrap of the unread period arrived, or zero
	// when no wrap waits unread.
	Since int64 `json:"since"`
	// Rang says that the relay rang in this unread period.
	Rang       bool  `json:"rang"`
	LastRing   int64 `json:"last_ring"`
	LastStatus int   `json:"last_status"`
	// Tries counts the tries of the current ring, and Next is when the
	// next one is due, or zero for none.
	Tries int   `json:"tries"`
	Next  int64 `json:"next"`
	// Failing is the time of the first failed try since the last 2xx
	// answer, or zero.
	Failing int64 `json:"failing"`

	Day  string `json:"day"`
	Used int    `json:"used"`
}

// read ends the unread period. A ring that waits for a retry is no longer
// needed: the citizen has the mail.
func (s *state) read(now time.Time) {
	s.LastRead = now.Unix()
	s.Since, s.Rang, s.Next = 0, false, 0
}

// wrap notes a relay-form wrap for the citizen. A wrap that arrives while
// a subscription of the citizen is open is read at once.
func (s *state) wrap(now time.Time, watching bool) {
	if watching {
		s.read(now)
		return
	}
	if s.Since == 0 {
		s.Since = now.Unix()
	}
}

// left is the number of rings that the day has left.
func (s *state) left(now time.Time) int {
	if s.Day != day(now) {
		return s.DailyCap
	}
	return max(s.DailyCap-s.Used, 0)
}

// due says whether the relay must try a ring now: the first ring of an
// unread period, or a retry of it.
func (s *state) due(now time.Time) bool {
	if s.Since == 0 || s.left(now) == 0 {
		return false
	}
	t := now.Unix()
	if s.Rang {
		return s.Next != 0 && t >= s.Next
	}
	if t < s.Since+int64(settle/time.Second) {
		return false
	}
	return s.LastRing == 0 || t-s.LastRing >= int64(s.MinInterval)
}

// try counts one try of a ring against the day.
func (s *state) try(now time.Time) {
	if s.Day != day(now) {
		s.Day, s.Used = day(now), 0
	}
	s.Used++
	s.LastRing = now.Unix()
	if !s.Rang {
		s.Rang, s.Tries = true, 0
	}
	s.Tries++
	s.Next = 0
}

// answer notes the HTTP status of a try, zero for no answer. A failed try
// plans the next retry, if one is left.
func (s *state) answer(status int, now time.Time) {
	s.LastStatus = status
	if status >= 200 && status < 300 {
		s.Failing, s.Next = 0, 0
		return
	}
	if s.Failing == 0 {
		s.Failing = now.Unix()
	}
	if s.Rang && s.Tries <= len(Retries) {
		s.Next = now.Add(Retries[s.Tries-1]).Unix()
	}
}

// abandoned says whether the relay must remove the registration.
func (s *state) abandoned(now time.Time) bool {
	return s.Failing != 0 && now.Unix()-s.Failing >= int64(Abandon/time.Second)
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }
