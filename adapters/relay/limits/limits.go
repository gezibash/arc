// Package limits makes a khatru relay refuse abuse, as docs/delivery/SPEC.md
// section 12 describes.
//
// A one-time key signs each gift wrap, so the relay cannot limit a gift wrap
// by its author. The relay therefore asks for NIP-42 authentication, or for
// NIP-13 proof of work, before it takes a gift wrap. It also caps the size of
// one event, the rate of events from one IP address, and the size of its
// store. Each limit is off when its value is zero.
package limits

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip13"
	"go.etcd.io/bbolt"
)

// WrapKinds are the kinds of a gift wrap: store and forward, and live.
var WrapKinds = []nostr.Kind{1059, 21059}

// Policy holds the limits of a relay.
type Policy struct {
	// MaxEventBytes caps the size of one event, as JSON.
	MaxEventBytes int
	// WrapAuth makes a gift wrap need NIP-42 authentication, or proof of
	// work of WrapPoW bits.
	WrapAuth bool
	// WrapPoW is the NIP-13 difficulty that replaces authentication. If it
	// is zero, only authentication lets a gift wrap in.
	WrapPoW int
	// Rate is how many events one IP address can write each minute. Burst
	// is how many it can write at once.
	Rate  int
	Burst int
	// IPHeader names the HTTP header that holds the IP address of the
	// client, when a proxy stands in front of the relay. If it is empty,
	// the relay uses the address of the connection.
	IPHeader string
	// MaxStoreBytes caps the bytes that the store uses.
	MaxStoreBytes int64
}

// Apply makes the relay keep the policy. The store is the database of the
// relay. Call Apply after the relay has its event store.
func Apply(rl *khatru.Relay, db *bbolt.DB, p Policy) {
	if p.MaxEventBytes > 0 && int64(p.MaxEventBytes) > rl.MaxMessageSize {
		rl.MaxMessageSize = int64(p.MaxEventBytes) + 1024
	}
	var bucket *buckets
	if p.Rate > 0 {
		bucket = newBuckets(p.Rate, p.Burst)
	}

	onEvent := rl.OnEvent
	rl.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if p.MaxEventBytes > 0 {
			if size := len(event.String()); size > p.MaxEventBytes {
				return true, fmt.Sprintf("invalid: the event holds %d bytes, and this relay takes at most %d", size, p.MaxEventBytes)
			}
		}
		if p.WrapAuth && isWrap(event.Kind) && len(khatru.GetAllAuthed(ctx)) == 0 &&
			(p.WrapPoW == 0 || nip13.Check(event.ID, p.WrapPoW) != nil) {
			if p.WrapPoW > 0 {
				return true, fmt.Sprintf("auth-required: a gift wrap needs authentication, or proof of work of %d bits", p.WrapPoW)
			}
			return true, "auth-required: a gift wrap needs authentication"
		}
		if bucket != nil && !bucket.take(clientIP(ctx, p.IPHeader)) {
			return true, "rate-limited: this address writes too many events"
		}
		if p.MaxStoreBytes > 0 && !event.Kind.IsEphemeral() && event.Kind != nostr.KindDeletion && used(db) >= p.MaxStoreBytes {
			return true, "error: the store of this relay is full"
		}
		if onEvent != nil {
			return onEvent(ctx, event)
		}
		return false, ""
	}
}

func isWrap(k nostr.Kind) bool {
	for _, w := range WrapKinds {
		if k == w {
			return true
		}
	}
	return false
}

// clientIP is the IP address of the client. A client can set any header, so
// the relay reads a header only when its operator names one that the proxy
// sets. It never reads X-Forwarded-For on its own.
func clientIP(ctx context.Context, header string) string {
	r := khatru.GetRequest(ctx)
	if r == nil {
		return ""
	}
	if header != "" {
		if ip := r.Header.Get(header); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// used is the bytes that the store holds: its size, less its free pages. A
// deletion frees pages, and the store uses them again, but the file does not
// shrink.
func used(db *bbolt.DB) int64 {
	var size int64
	db.View(func(tx *bbolt.Tx) error {
		size = tx.Size()
		return nil
	})
	return size - int64(db.Stats().FreeAlloc)
}

// buckets holds one token bucket for each IP address.
type buckets struct {
	mu    sync.Mutex
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time
	by    map[string]*bucket
	swept time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newBuckets(perMinute, burst int) *buckets {
	if burst < 1 {
		burst = perMinute
	}
	return &buckets{rate: float64(perMinute) / 60, burst: float64(burst), now: time.Now, by: map[string]*bucket{}}
}

// take takes one token for the address. It says false when the bucket is
// empty.
func (b *buckets) take(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.sweep(now)

	k, ok := b.by[ip]
	if !ok {
		k = &bucket{tokens: b.burst, last: now}
		b.by[ip] = k
	}
	k.tokens = min(b.burst, k.tokens+now.Sub(k.last).Seconds()*b.rate)
	k.last = now
	if k.tokens < 1 {
		return false
	}
	k.tokens--
	return true
}

// sweep forgets the buckets that are full again, once each minute, so the
// map does not grow with each address that ever wrote.
func (b *buckets) sweep(now time.Time) {
	if now.Sub(b.swept) < time.Minute {
		return
	}
	b.swept = now
	for ip, k := range b.by {
		if k.tokens+now.Sub(k.last).Seconds()*b.rate >= b.burst {
			delete(b.by, ip)
		}
	}
}
