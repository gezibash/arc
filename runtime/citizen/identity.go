package citizen

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/runtime/iface"
)

// RootFilter matches the draft that holds a citizen's keyed root.
func RootFilter(me nostr.PubKey) nostr.Filter {
	return nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{me}, Tags: nostr.TagMap{"d": {iface.KeyedRootD}}}
}

// publishKeyedRoot seals the keyed root to this citizen's own key, once, so
// that a machine that signs through NIP-46 can read it. The root follows
// from the secret key, so every machine with the key makes the same one.
func (sess *Session) PublishKeyedRoot(ctx context.Context) error {
	held, err := sess.Node.Store.Query(RootFilter(sess.Key.Public))
	if err != nil {
		return err
	}
	if len(held) > 0 {
		return nil
	}
	root, err := iface.KeyedRoot(sess.Key.Secret)
	if err != nil {
		return err
	}
	inner := nostr.Event{Kind: iface.KeyedRootKind, CreatedAt: nostr.Now(), Content: hex.EncodeToString(root),
		Tags: nostr.Tags{{"d", iface.KeyedRootD}}}
	wrap, err := draft.Wrap(ctx, sess.Signer, iface.KeyedRootD, inner, nostr.Now())
	if err != nil {
		return err
	}
	_, _, err = sess.Node.Publish(ctx, wrap, sess.Relays)
	return err
}

// keyedRoot is the root of this citizen's keyed values. A machine with the
// key derives it. A machine that signs through NIP-46 opens the draft that
// holds it, through the signer.
func (sess *Session) KeyedRoot(ctx context.Context) ([]byte, error) {
	if !sess.Remote {
		return iface.KeyedRoot(sess.Key.Secret)
	}
	filter := RootFilter(sess.Key.Public)
	reports, errs := sess.Node.Pull(ctx, filter, sess.Relays)
	wraps, err := sess.Node.Store.Query(filter)
	if err != nil {
		return nil, err
	}
	if unreached := node.Unreached(reports, errs); len(wraps) == 0 && unreached != nil {
		return nil, fmt.Errorf("this machine holds no keyed root, and %w", unreached)
	}
	if len(wraps) == 0 {
		return nil, errors.New("no keyed root yet: run any arc command on a machine that holds the key, then sync")
	}
	opened, err := draft.Open(ctx, sess.Signer, wraps[0])
	if err != nil {
		return nil, fmt.Errorf("the keyed root does not open: %w", err)
	}
	root, err := hex.DecodeString(opened.Event.Content)
	if err != nil || len(root) != 32 || opened.Event.Kind != iface.KeyedRootKind {
		return nil, errors.New("the keyed root draft holds no root")
	}
	return root, nil
}
