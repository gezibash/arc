package citizen

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// InboxWatch is what WatchInbox prints, and from when.
type InboxWatch struct {
	// Since is an RFC 3339 time or the id of a message. WatchInbox first
	// shows the stored messages from that point. With none, it shows no
	// stored message.
	Since string
	// From limits the messages to these senders. With none, it shows the
	// messages of each sender.
	From map[nostr.PubKey]bool
	// Show prints one message.
	Show func(mail.Message) error
	// Log says when each relay watches, and when a watch ends.
	Log *slog.Logger
}

// WatchInbox holds a watch on each relay, and shows each new message, until
// ctx ends. It never marks a message read. It changes the store of the
// session, so the session serves nothing else while it runs.
func (sess *Session) WatchInbox(ctx context.Context, w InboxWatch) error {
	var relays []transport.Live
	for _, t := range sess.Relays {
		if live, ok := t.(transport.Live); ok {
			relays = append(relays, live)
		}
	}
	if len(relays) == 0 {
		return errors.New("arc message watch needs a relay: run arc relay add <url>")
	}

	// The mail of a watch saves each seal that it opens. After each save,
	// the inbox can hold a new message.
	saved := make(chan struct{}, 1)
	sess.Node.Store = sealNotice{EventStore: sess.Node.Store, saved: saved}

	shown := &inboxShown{sess: sess, w: w, seen: map[string]bool{}}
	msgs, err := sess.Mail.Inbox(ctx)
	if err != nil {
		return err
	}
	after, err := sess.sinceFilter(ctx, w.Since, msgs)
	if err != nil {
		return err
	}
	if err := shown.showNew(ctx, msgs, after); err != nil {
		return err
	}

	// The store closes after the watches end.
	var watches sync.WaitGroup
	defer watches.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, r := range relays {
		watches.Go(func() {
			sess.KeepMail(ctx, r, func() { w.Log.Info("watching", "relay", r.Name()) }, w.Log)
		})
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-saved:
		}
		msgs, err := sess.Mail.Inbox(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if err := shown.showNew(ctx, msgs, func(mail.Message) bool { return true }); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// inboxShown remembers the messages that a watch showed.
type inboxShown struct {
	sess *Session
	w    InboxWatch
	seen map[string]bool
}

// showNew prints the messages that it did not print before, in the order
// of receipt. An adapter that gives the last id to --since after a restart
// then loses no message.
func (s *inboxShown) showNew(ctx context.Context, msgs []mail.Message, include func(mail.Message) bool) error {
	var fresh []mail.Message
	for _, m := range msgs {
		if !s.seen[m.ID] {
			s.seen[m.ID] = true
			if include(m) {
				fresh = append(fresh, m)
			}
		}
	}
	if len(fresh) > 1 {
		order, err := s.sess.ReceiptOrder(ctx)
		if err != nil {
			return err
		}
		// A message with no receipt comes first, in the order of the inbox.
		slices.SortStableFunc(fresh, func(a, b mail.Message) int { return cmp.Compare(order[a.ID], order[b.ID]) })
	}
	for _, m := range fresh {
		if len(s.w.From) > 0 && !s.w.From[m.From] {
			continue
		}
		if err := s.w.Show(m); err != nil {
			return err
		}
	}
	return nil
}

// sinceFilter says which stored messages watch prints before it watches.
// With no point, it prints none. A time includes the messages at that time,
// by the clock of each sender. A message id selects the messages that this
// machine stored after that message, and each message that it stored before
// it kept receipts.
func (sess *Session) sinceFilter(ctx context.Context, since string, msgs []mail.Message) (func(mail.Message) bool, error) {
	if since == "" {
		return func(mail.Message) bool { return false }, nil
	}
	if at, err := time.Parse(time.RFC3339, since); err == nil {
		return func(m mail.Message) bool { return !m.At.Before(at) }, nil
	}
	if !slices.ContainsFunc(msgs, func(m mail.Message) bool { return m.ID == since }) {
		return nil, fmt.Errorf("--since %q is not an RFC 3339 time or the id of a message in the inbox", since)
	}
	order, err := sess.ReceiptOrder(ctx)
	if err != nil {
		return nil, err
	}
	mark := order[since]
	return func(m mail.Message) bool {
		position, ok := order[m.ID]
		return m.ID != since && (!ok || position > mark)
	}, nil
}

// sealNotice tells a watch that the store saved a seal. A seal that the store
// held already also counts: another arc command can save a message first.
type sealNotice struct {
	node.EventStore
	saved chan<- struct{}
}

func (s sealNotice) Save(event nostr.Event) (store.Result, error) {
	result, err := s.EventStore.Save(event)
	if event.Kind == private.SealKind {
		select {
		case s.saved <- struct{}{}:
		default:
		}
	}
	return result, err
}
