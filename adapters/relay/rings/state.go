package rings

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"
)

// MaxAttempts is the number of tries of one ring: the first, and three
// retries (docs/rings/SPEC.md, section 3.7).
const MaxAttempts = 4

// retryBase is the wait before the first retry. Each later retry waits four
// times longer: 30 s, 2 min, 8 min, before the jitter.
const retryBase = 30 * time.Second

// Abandon is how long a registration lives with no 2xx answer, after its
// first failed try.
const Abandon = 7 * 24 * time.Hour

// job is a ring that the relay started, and that has not ended. The store
// keeps it, so a ring outlives a restart of the relay.
type job struct {
	// ID names the ring in its body and header, the same on each try.
	ID string `json:"id"`
	// Since is the since of the body.
	Since int64 `json:"since"`
	// Attempt counts the tries that began.
	Attempt int `json:"attempt"`
	// Next is the Unix time in milliseconds of the next try, zero for now.
	Next int64 `json:"next"`
}

// state is what the relay knows of one registered citizen. Times are Unix
// seconds unless a name says otherwise; zero is never.
type state struct {
	Settings
	// SealedKey is the ring key, encrypted with the secret of the relay.
	SealedKey []byte `json:"sealed_key"`
	// Relay is the wss URL of this relay, as the citizen named it.
	Relay   string `json:"relay"`
	Created int64  `json:"created"`
	// Expires is when the registration ends, unless the citizen reads or
	// sets it again.
	Expires int64 `json:"expires"`

	// LastRead is the time of the last read.
	LastRead int64 `json:"last_read"`
	// Since is when the first wrap of the unread period arrived, or zero
	// when no wrap waits unread, and SinceWrap is the ID of that wrap.
	Since     int64  `json:"since"`
	SinceWrap string `json:"since_wrap,omitempty"`
	// Rang says that the relay started a ring in this unread period.
	Rang bool `json:"rang"`
	// Job is the ring that has not ended, or nil.
	Job *job `json:"job,omitempty"`

	// Tokens is the content of the bucket at Refilled, in Unix
	// milliseconds. A ring takes one token. A token comes back each
	// MinInterval seconds, up to Burst.
	Tokens   float64 `json:"tokens"`
	Refilled int64   `json:"refilled_ms"`

	LastRing   int64 `json:"last_ring"`
	LastStatus int   `json:"last_status"`
	// Failing is the time of the first failed try since the last 2xx
	// answer, or zero, and Failures counts the failed tries since then.
	Failing  int64 `json:"failing"`
	Failures int   `json:"failures,omitempty"`

	Day  string `json:"day"`
	Used int    `json:"used"`
}

// step is what the relay does next for a citizen.
type step int

const (
	idle step = iota // nothing waits
	wait             // a ring waits until a time
	send             // a ring goes now
)

// fill brings the bucket to now.
func (s *state) fill(now time.Time) {
	ms := now.UnixMilli()
	if s.Refilled == 0 {
		s.Tokens, s.Refilled = float64(s.Burst), ms
		return
	}
	if ms <= s.Refilled {
		return
	}
	per := float64(s.MinInterval) * 1000
	s.Tokens = math.Min(float64(s.Burst), s.Tokens+float64(ms-s.Refilled)/per)
	s.Refilled = ms
}

// tokenAt is when the bucket holds one token again, rounded up to the
// millisecond in which the bucket counts.
func (s *state) tokenAt(now time.Time) time.Time {
	ms := math.Ceil((1 - s.Tokens) * float64(s.MinInterval) * 1000)
	return time.UnixMilli(now.UnixMilli() + int64(ms))
}

// read ends the unread period. A ring that waits for a retry is no longer
// needed: the citizen has the mail.
func (s *state) read(now time.Time) {
	s.LastRead = now.Unix()
	s.Since, s.SinceWrap, s.Rang, s.Job = 0, "", false, nil
}

// wrap notes a relay-form wrap for the citizen. A wrap that arrives while
// a subscription of the citizen is open is read at once.
func (s *state) wrap(now time.Time, watching bool, wrapID string) {
	if watching {
		s.read(now)
		return
	}
	if s.Since == 0 {
		s.Since, s.SinceWrap = now.Unix(), wrapID
	}
}

// RingID is the ID of the ring for the unread period that a wrap started:
// the first 16 bytes of SHA-256 of "arc-ring-v1", a zero byte, the public
// key of the citizen in hex, and the ID of the wrap in hex. Each relay that
// took the wrap gives the ring the same ID, so a URL can drop the rings of
// the other relays.
func RingID(citizen, wrapID string) string {
	sum := sha256.Sum256([]byte("arc-ring-v1\x00" + citizen + wrapID))
	return hex.EncodeToString(sum[:16])
}

// left is the number of rings that the day has left.
func (s *state) left(now time.Time) int {
	if s.Day != day(now) {
		return s.DailyCap
	}
	return max(s.DailyCap-s.Used, 0)
}

func (s *state) expired(now time.Time) bool { return s.Expires != 0 && now.Unix() >= s.Expires }

// renew moves the end of the registration to ttl after now: a citizen that
// reads keeps its rings, and one that stops reading loses them after ttl.
func (s *state) renew(now time.Time, ttl time.Duration) {
	s.Expires = max(s.Expires, now.Add(ttl).Unix())
}

// The reasons that the log gives when a ring does not go now
// (docs/rings/SPEC.md, sections 3.5 and 3.7).
const (
	whyNoRegistration = "no registration"
	whyExpired        = "expired"
	whyWatching       = "a read subscription is open"
	whyRang           = "already rang in this unread period"
	whyOnTheWay       = "a try is on the way"
	whyRetry          = "a retry waits"
	whyDailyCap       = "daily cap reached, waits"
	whyCitizenBucket  = "citizen bucket empty, waits"
	whyRelayBucket    = "relay bucket empty, waits"
)

// next says what the relay does next, and when, for a wait.
func (s *state) next(now time.Time) (step, time.Time) {
	what, at, _ := s.plan(now)
	return what, at
}

// plan is next, with the reason that no ring goes now. The reason is
// empty when a ring goes, or when no wrap waits unread.
func (s *state) plan(now time.Time) (step, time.Time, string) {
	if s.Since == 0 {
		return idle, time.Time{}, ""
	}
	if s.expired(now) {
		return idle, time.Time{}, whyExpired
	}
	if s.Job == nil && s.Rang {
		return idle, time.Time{}, whyRang
	}
	if s.Job != nil && s.Job.Next > now.UnixMilli() {
		return wait, time.UnixMilli(s.Job.Next), whyRetry
	}
	if s.left(now) == 0 {
		return wait, midnight(now), whyDailyCap
	}
	if s.Job == nil {
		s.fill(now)
		if s.Tokens < 1 {
			return wait, s.tokenAt(now), whyCitizenBucket
		}
	}
	return send, time.Time{}, ""
}

// begin starts a try. The first try of a ring takes a token and makes the
// job, with the ID that the first wrap gives, or else id. Each try counts
// against the day.
func (s *state) begin(now time.Time, citizen, id string) job {
	if s.Job == nil {
		if s.SinceWrap != "" {
			id = RingID(citizen, s.SinceWrap)
		}
		s.fill(now)
		s.Tokens--
		s.Rang = true
		s.Job = &job{ID: id, Since: s.Since}
	}
	s.count(now)
	s.Job.Attempt++
	s.Job.Next = 0
	return *s.Job
}

// count counts one ring against the day.
func (s *state) count(now time.Time) {
	if s.Day != day(now) {
		s.Day, s.Used = day(now), 0
	}
	s.Used++
	s.LastRing = now.Unix()
}

// finish notes the HTTP status of a try, zero for no answer. A failed try
// plans a retry with backoff and jitter, while tries are left, unless the
// answer says that a retry cannot help. jitter is in [0, 1).
func (s *state) finish(status int, now time.Time, jitter float64) {
	s.LastStatus = status
	if status >= 200 && status < 300 {
		s.Failing, s.Failures, s.Job = 0, 0, nil
		return
	}
	if s.Failing == 0 {
		s.Failing = now.Unix()
	}
	s.Failures++
	if s.Job == nil {
		return
	}
	if s.Job.Attempt >= MaxAttempts || final(status) {
		s.Job = nil
		return
	}
	s.Job.Next = now.Add(backoff(s.Job.Attempt, jitter)).UnixMilli()
}

// final says whether an answer refuses the ring for good: a 4xx answer,
// such as 401 for a key that the platform no longer takes. 408 and 429 ask
// to try again.
func final(status int) bool {
	return status >= 400 && status < 500 && status != 408 && status != 429
}

// failure is the text that tells a citizen that its rings fail, or empty
// when they do not.
func (s *state) failure(host string) string {
	if s.Failing == 0 {
		return ""
	}
	answer := "no answer"
	if s.LastStatus != 0 {
		answer = fmt.Sprintf("answer %d", s.LastStatus)
	}
	return fmt.Sprintf("arc-ring: the rings to %s fail since %s UTC: last %s, %d failed tries; check the key, then run arc relay ring set",
		host, time.Unix(s.Failing, 0).UTC().Format("2006-01-02 15:04"), answer, s.Failures)
}

// lowRings is the number of rings left in a day at which a read is told.
const lowRings = 5

// low is the text that tells a citizen that the day has few rings left, or
// empty. A day in which no ring was used says nothing, so that a small
// daily_cap does not tell each read.
func (s *state) low(now time.Time) string {
	left := s.left(now)
	switch {
	case left > lowRings || left == s.DailyCap:
		return ""
	case left == 0:
		return fmt.Sprintf("arc-ring: 0 of %d rings left today; rings wait until 00:00 UTC", s.DailyCap)
	}
	return fmt.Sprintf("arc-ring: %d of %d rings left today; then rings wait until 00:00 UTC", left, s.DailyCap)
}

// backoff is the wait after a failed try: half the base wait, plus a random
// part of the other half, so that retries of many citizens spread out.
func backoff(attempt int, jitter float64) time.Duration {
	d := retryBase << (2 * (attempt - 1))
	return d/2 + time.Duration(jitter*float64(d/2))
}

// abandoned says whether the relay must remove the registration.
func (s *state) abandoned(now time.Time) bool {
	return s.Failing != 0 && now.Unix()-s.Failing >= int64(Abandon/time.Second)
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func midnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}
