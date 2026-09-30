package citizen

import (
	"context"
	"errors"
	"slices"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/relaylist"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/catalog"
)

// online says whether a citizen has a current announcement on the relays of
// this citizen, or on the read relays of the provider. If no relay answers,
// it returns the error, and the call goes ahead.
func (s *Session) online(ctx context.Context, citizen []byte) (bool, error) {
	provider := nostr.PubKey(citizen)
	filter := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}}
	targets, err := s.liveRelays(ctx, provider)
	if err != nil {
		return false, err
	}
	if err := node.Unreached(s.Node.Pull(ctx, filter, targets)); err != nil {
		return false, err
	}
	now := time.Now()
	announcements, err := s.Node.Store.Query(filter)
	if err != nil {
		return false, err
	}
	for _, announcement := range announcements {
		if catalog.Current(announcement, now) {
			return true, nil
		}
	}
	return false, nil
}

// liveRelays returns the relays that a live call to a provider tries: the
// relays of this citizen, then each read relay of the provider's NIP-65 list
// that this citizen does not use. See docs/delivery/SPEC.md, section 4.10.
func (s *Session) liveRelays(ctx context.Context, provider nostr.PubKey) ([]transport.Transport, error) {
	out := append([]transport.Transport(nil), s.Relays...)
	urls, err := relaylist.ReadRelays(ctx, s.Node, provider, slices.Concat(s.Relays, s.Indexers))
	if err != nil {
		return nil, err
	}
	for _, url := range urls {
		known := slices.ContainsFunc(out, func(t transport.Transport) bool {
			return relaylist.Same(t.Name(), url)
		})
		if !known {
			out = append(out, s.NewRelay(url))
		}
	}
	return out, nil
}

// liveCall tries each relay in turn, and returns the first reply, the round
// trip, and the relay that carried it.
func (sess *Session) LiveCall(ctx context.Context, provider nostr.PubKey, request call.Request, timeout time.Duration) (call.Reply, time.Duration, string, error) {
	ctx, cancel := context.WithTimeout(ctx, min(timeout, call.Timeout))
	defer cancel()
	if len(sess.Relays) == 0 {
		return call.Reply{}, 0, "", errors.New("no relay, so no live path: add a relay, or use --later to store and forward the call")
	}
	if err := sess.Waker.Wake(ctx, provider[:], sess.online); err != nil {
		return call.Reply{}, 0, "", err
	}
	targets, err := sess.liveRelays(ctx, provider)
	if err != nil {
		return call.Reply{}, 0, "", err
	}
	reply, rtt, path, err := LiveCall(ctx, sess.Signer, provider, request, targets, timeout)
	if err == nil {
		sess.Waker.Answered(provider[:])
	}
	return reply, rtt, path, err
}
