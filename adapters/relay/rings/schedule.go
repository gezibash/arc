package rings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"fiatjaf.com/nostr"
)

// schedule does what is next for a citizen: it starts a try now, sets one
// timer for a ring that must wait, or does nothing. It returns the reason
// that no try starts now, empty when one starts or no wrap waits, and the
// wait of a ring that waits. The caller holds r.mu.
func (r *Rings) schedule(pk nostr.PubKey) (string, time.Duration) {
	s := r.by[pk]
	if s == nil {
		return whyNoRegistration, 0
	}
	if r.busy[pk] {
		return whyOnTheWay, 0
	}
	r.stop(pk)
	now := r.cfg.Now()
	what, at, why := s.plan(now)
	if what == send {
		if delay, ok := r.relayToken(now); !ok {
			what, at, why = wait, now.Add(delay), whyRelayBucket
		}
	}
	switch what {
	case send:
		r.start(pk, s, now)
	case wait:
		r.timers[pk] = time.AfterFunc(max(at.Sub(now), 0), func() { r.wake(pk) })
		return why, max(at.Sub(now), 0)
	}
	return why, 0
}

// wake runs when the timer of a citizen fires.
func (r *Rings) wake(pk nostr.PubKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.timers, pk)
	r.schedule(pk)
}

// stop ends the timer of a citizen. The caller holds r.mu.
func (r *Rings) stop(pk nostr.PubKey) {
	if t := r.timers[pk]; t != nil {
		t.Stop()
		delete(r.timers, pk)
	}
}

// relayToken takes one token of the bucket of the whole relay, or says how
// long until one comes back. The caller holds r.mu.
func (r *Rings) relayToken(now time.Time) (time.Duration, bool) {
	perSecond := float64(r.cfg.PerMinute) / 60
	if elapsed := now.Sub(r.relayRefilled).Seconds(); elapsed > 0 {
		r.relayTokens = math.Min(float64(r.cfg.PerMinute), r.relayTokens+elapsed*perSecond)
		r.relayRefilled = now
	}
	if r.relayTokens < 1 {
		return time.Duration((1 - r.relayTokens) / perSecond * float64(time.Second)), false
	}
	r.relayTokens--
	return 0, true
}

// start begins a try, keeps it in the store, and sends it. The caller holds
// r.mu.
func (r *Rings) start(pk nostr.PubKey, s *state, now time.Time) {
	try := s.begin(now, pk.Hex(), ringID())
	r.save(pk, s)
	r.busy[pk] = true
	snapshot := *s
	r.sends.Go(func() {
		status, err := r.ring(context.Background(), pk, snapshot, try)
		// An error can name the host of the URL, so the log says only that
		// the ring failed.
		r.cfg.Logger.Info("ring", "citizen", short(pk), "status", status, "failed", err != nil, "attempt", try.Attempt)
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.busy, pk)
		s := r.by[pk]
		if s == nil {
			return
		}
		if s.Job != nil && s.Job.ID == try.ID {
			s.finish(status, r.cfg.Now(), randFloat())
		} else {
			s.LastStatus = status
		}
		r.save(pk, s)
		r.schedule(pk)
	})
}

// ring makes one call to the ring URL of a citizen, and returns the HTTP
// status, zero for no answer.
func (r *Rings) ring(ctx context.Context, pk nostr.PubKey, s state, try job) (int, error) {
	key, err := r.open(s.SealedKey)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(call{Type: "arc.mail", ID: try.ID, Citizen: pk.Hex(), Relay: s.Relay, Since: try.Since})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+string(key))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "arc-relay-rings")
	request.Header.Set(IDHeader, try.ID)
	response, err := r.client.Do(request)
	if err != nil {
		// The error of the client names the URL, which can hold a secret.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, answerBytes))
	_ = response.Body.Close()
	return response.StatusCode, nil
}

// ringID is a random ID of 16 bytes, in hex.
func ringID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// randFloat is a random number in [0, 1), for the jitter of a retry.
func randFloat() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}
