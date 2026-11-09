package mail

import (
	"context"
	"fmt"
	"slices"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// Report says what one sync of mail did.
type Report struct {
	Transport string
	// Received counts messages for this citizen that arrived.
	Received int
	// Answered counts calls to this citizen that it answered.
	Answered int
	// Replies counts replies to this citizen's calls that arrived.
	Replies int
	// Delivered counts messages of this citizen that the recipient
	// acknowledged.
	Delivered int
	// Carried counts wraps for other citizens that this node took on.
	Carried int
	// Held counts messages from strangers, without work, that this node
	// held apart from the inbox.
	Held int
	// Sent counts wraps that this node gave to the transport.
	Sent    int
	Refused []string
}

// Sync moves mail through one transport. It takes the mail for this citizen,
// opens it, and acknowledges each message. On a carrier, it also takes the
// courier form of other citizens' mail while the hop limit allows. Then it
// gives the transport every wrap that this node moves.
func (m *Mail) Sync(ctx context.Context, t transport.Transport) (Report, error) {
	report := Report{Transport: t.Name()}
	m.receiving.Lock()
	err := m.settle(ctx, &report)
	m.receiving.Unlock()
	if err != nil {
		return report, err
	}

	carrier, isCarrier := t.(transport.Carrier)
	mine := private.RouteTags(m.key.PublicKey(), m.now())
	filters := []nostr.Filter{{Kinds: []nostr.Kind{private.WrapKind}}}
	if !isCarrier {
		filters = m.filters(mine)
	}

	// The fetch holds no lock, so a watch can take mail while it runs.
	var batches []transport.Batch
	for _, filter := range filters {
		batch, err := t.Fetch(ctx, filter)
		if err != nil {
			return report, err
		}
		batches = append(batches, batch)
	}

	m.receiving.Lock()
	defer m.receiving.Unlock()
	for _, batch := range batches {
		for _, wrap := range batch.Events {
			if err := m.take(ctx, wrap, mine, isCarrier, batch.Hops, &report); err != nil {
				return report, err
			}
		}
	}
	if err := m.evict(); err != nil {
		return report, err
	}
	return report, m.give(ctx, t, carrier, isCarrier, &report)
}

// Flush does the work of a sync that needs no fetch. It ends expired mail,
// answers each pending call, and gives a relay each wrap that the relay does
// not hold yet. A watch takes the mail, so a relay that a watch covers needs
// only a flush. Flush sends nothing when the relay holds each wrap already.
func (m *Mail) Flush(ctx context.Context, t transport.Transport) (Report, error) {
	report := Report{Transport: t.Name()}
	m.receiving.Lock()
	defer m.receiving.Unlock()
	if err := m.settle(ctx, &report); err != nil {
		return report, err
	}
	if err := m.evict(); err != nil {
		return report, err
	}
	carrier, isCarrier := t.(transport.Carrier)
	return report, m.give(ctx, t, carrier, isCarrier, &report)
}

// Watch takes the mail for this citizen from a live transport as it arrives.
// It watches the filters that a sync fetches, flushes, and then calls
// ready. The transport sends the mail that it holds first, then each new
// wrap. Watch returns nil at the next midnight UTC, because the route tags
// change each day. It returns an error when the transport ends a watch, or
// when the context ends.
func (m *Mail) Watch(ctx context.Context, t transport.Live, ready func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	now := m.now()
	mine := private.RouteTags(m.key.PublicKey(), now)
	var watches []<-chan nostr.Event
	for _, filter := range m.filters(mine) {
		events, err := t.Watch(ctx, filter)
		if err != nil {
			return err
		}
		watches = append(watches, events)
	}
	if _, err := m.Flush(ctx, t); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}

	midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	day := time.NewTimer(midnight.Sub(now))
	defer day.Stop()
	for {
		var wrap nostr.Event
		var open bool
		select {
		case <-day.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case wrap, open = <-watches[0]:
		case wrap, open = <-watches[1]:
		}
		if !open {
			return fmt.Errorf("mail: %s ended the watch", t.Name())
		}
		report := Report{Transport: t.Name()}
		m.receiving.Lock()
		err := m.take(ctx, wrap, mine, false, nil, &report)
		m.receiving.Unlock()
		if err != nil {
			return err
		}
	}
}

// filters are the two filters of the mail for this citizen: by its key, and
// by its route tags. One filter cannot hold both, because a filter joins
// its tags with AND.
func (m *Mail) filters(mine []string) []nostr.Filter {
	return []nostr.Filter{
		{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {m.key.PublicKey().Hex()}}},
		{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"w": mine}},
	}
}

// settle ends expired mail, releases the held mail of contacts, and answers
// each pending call. The caller holds the receiving lock.
func (m *Mail) settle(ctx context.Context, report *Report) error {
	if err := m.expire(); err != nil {
		return err
	}
	if err := m.seed(ctx); err != nil {
		return err
	}
	if err := m.release(ctx, report); err != nil {
		return err
	}
	return m.pending(ctx, report)
}

// take handles one wrap that a transport held.
func (m *Mail) take(ctx context.Context, wrap nostr.Event, mine []string, isCarrier bool, hops map[nostr.ID]int, report *Report) error {
	if err := store.Verify(wrap); err != nil {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": "+err.Error())
		return nil
	}
	if store.Expired(wrap, m.now()) {
		return nil
	}

	if forMe(wrap, m.key.PublicKey(), mine) {
		return m.open(ctx, wrap, report)
	}
	if isCarrier {
		return m.carry(wrap, hops[wrap.ID], report)
	}
	return nil
}

func forMe(wrap nostr.Event, me nostr.PubKey, mine []string) bool {
	if tag := wrap.Tags.Find("p"); len(tag) > 1 && tag[1] == me.Hex() {
		return true
	}
	tag := wrap.Tags.Find("w")
	return len(tag) > 1 && slices.Contains(mine, tag[1])
}

// open opens a wrap for this citizen, keeps its seal, and acts on the rumor:
// an acknowledgement marks a message delivered, and a message is
// acknowledged. Receipt of a request is separate from its execution journal.
func (m *Mail) open(ctx context.Context, wrap nostr.Event, report *Report) error {
	opened, err := private.Unwrap(ctx, m.key, wrap)
	if err != nil {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": "+err.Error())
		return nil
	}
	if opened.Rumor.Kind == call.RequestKind {
		previouslyReceived, err := m.node.Store.Has(opened.Seal.ID)
		if err != nil {
			return err
		}
		return m.receiveRequest(ctx, opened, previouslyReceived, report)
	}
	if opened.Rumor.Kind == MessageKind && !names(opened.Rumor, m.key.PublicKey()) {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": the message names this citizen in no p tag")
		return nil
	}
	if filtered(opened.Rumor.Kind) && m.Contacts != nil {
		if err := m.wrote(opened.Author()); err != nil {
			return err
		}
		// Another wrap of a seal in the store, such as its courier form, is
		// a duplicate below.
		stored, err := m.node.Store.Has(opened.Seal.ID)
		if err != nil {
			return err
		}
		admitted := stored
		if !stored {
			admitted, err = m.admits(ctx, opened.Author(), wrap)
		}
		if err != nil {
			return err
		}
		if !admitted {
			return m.hold(opened, wrap, report)
		}
		if err := m.unhold(opened.Seal.ID); err != nil {
			return err
		}
	}

	result, err := m.node.Store.Save(opened.Seal)
	if err != nil {
		return err
	}
	if result.Outcome == store.Duplicate && opened.Rumor.Kind != AckKind && opened.Rumor.Kind != call.ReplyKind {
		return nil
	}
	if result.Outcome == store.Refused {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": "+result.Reason)
		return nil
	}

	switch opened.Rumor.Kind {
	case AckKind:
		return m.acknowledged(opened, report)
	case call.ReplyKind:
		return m.replied(opened, report)
	default:
		// A message, or another private kind of a capability.
		report.Received++
		ack := private.Rumor(m.key, AckKind, "", nostr.Tags{{"e", opened.Rumor.ID.Hex()}}, m.now())
		_, err := m.post(ctx, opened.Author(), ack, m.now().Add(private.MaxAge))
		return err
	}
}
