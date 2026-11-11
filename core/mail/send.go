package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// Send seals a message to each recipient, puts it in the outbox, and sends
// it to the relays at once. With more than one recipient, it is a group
// message: one rumor with a p tag for each recipient. A relay that fails
// does not fail the send: the outbox sends the message again on the next
// sync. Send returns one outbox entry for each recipient.
func (m *Mail) Send(ctx context.Context, to []nostr.PubKey, text string) ([]Outgoing, error) {
	tags := make(nostr.Tags, 0, len(to))
	for _, pk := range to {
		tags = append(tags, nostr.Tag{"p", pk.Hex()})
	}
	return m.SendRumor(ctx, to, MessageKind, text, tags)
}

// SendReply sends a message that replies to a parent, as Send does. The
// rumor holds an e tag that names the parent. The parent must be a message
// that this citizen received or sent, and the reply must have its members.
func (m *Mail) SendReply(ctx context.Context, to []nostr.PubKey, parent nostr.ID, text string) ([]Outgoing, error) {
	if err := m.CheckReply(ctx, to, parent); err != nil {
		return nil, err
	}
	tags := make(nostr.Tags, 0, len(to)+1)
	for _, pk := range to {
		tags = append(tags, nostr.Tag{"p", pk.Hex()})
	}
	tags = append(tags, nostr.Tag{"e", parent.Hex()})
	return m.SendRumor(ctx, to, MessageKind, text, tags)
}

// CheckReply refuses a reply that SendReply refuses: a parent that this
// citizen did not receive or send, or recipients that are not the members
// of the parent. A command can check before it reads the text.
func (m *Mail) CheckReply(ctx context.Context, to []nostr.PubKey, parent nostr.ID) error {
	if err := m.checkRecipients(to); err != nil {
		return err
	}
	rumors, err := m.Rumors(ctx, []nostr.Kind{MessageKind})
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rumors, func(r nostr.Event) bool { return r.ID == parent })
	if i < 0 {
		return fmt.Errorf("mail: no message %s on this device", parent.Hex())
	}
	if !sameMembers(Members(rumors[i]), append([]nostr.PubKey{m.key.PublicKey()}, to...)) {
		return errors.New("mail: the reply goes to the members of its parent")
	}
	return nil
}

// sameMembers says whether two lists hold the same keys, in any order.
func sameMembers(a, b []nostr.PubKey) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	compare := func(x, y nostr.PubKey) int { return bytes.Compare(x[:], y[:]) }
	slices.SortFunc(a, compare)
	slices.SortFunc(b, compare)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// SendRumor seals one private event of any kind to each recipient, as Send
// does with a message. The caller gives the tags.
func (m *Mail) SendRumor(ctx context.Context, to []nostr.PubKey, kind nostr.Kind, content string, tags nostr.Tags) ([]Outgoing, error) {
	if err := m.checkRecipients(to); err != nil {
		return nil, err
	}
	switch kind {
	case AckKind, call.RequestKind, call.ReplyKind:
		return nil, fmt.Errorf("mail: kind %d belongs to the mail layer", kind)
	}

	if m.Contacts != nil {
		if err := m.Contacts.AddContacts(ctx, to); err != nil {
			return nil, err
		}
	}
	now := m.now()
	rumor := private.Rumor(m.key, kind, content, tags, now)
	return m.queue(ctx, to, rumor, "message")
}

// checkRecipients refuses no recipient, this citizen, a recipient named
// twice, and more than MaxParticipants with this citizen.
func (m *Mail) checkRecipients(to []nostr.PubKey) error {
	if len(to) == 0 {
		return errors.New("mail: a message needs a recipient")
	}
	if len(to)+1 > MaxParticipants {
		return fmt.Errorf("mail: a group message has at most %d participants, with you; this one has %d", MaxParticipants, len(to)+1)
	}
	seen := map[nostr.PubKey]bool{}
	for _, pk := range to {
		if pk == m.key.PublicKey() {
			return errors.New("mail: a message to yourself belongs in the journal")
		}
		if seen[pk] {
			return fmt.Errorf("mail: %s is a recipient twice", pk.Hex())
		}
		seen[pk] = true
	}
	return nil
}

// Request commits a call to the outbox before attempting any network send.
func (m *Mail) Request(ctx context.Context, provider nostr.PubKey, r call.Request) (Outgoing, error) {
	r.DeadlineMS = 0 // A deferred request's execution budget starts when served.
	out, err := m.queue(ctx, []nostr.PubKey{provider}, call.RequestRumor(m.key, provider, r, m.now()), "request")
	if err != nil {
		return Outgoing{}, err
	}
	return out[0], nil
}

// queue seals one rumor to each recipient, and commits each outbox entry
// and wrap in one transaction before any network send.
func (m *Mail) queue(ctx context.Context, to []nostr.PubKey, rumor nostr.Event, kind string) ([]Outgoing, error) {
	if err := private.CheckMail(rumor); err != nil {
		return nil, err
	}
	expires := m.now().Add(private.MaxAge)
	outs := make([]Outgoing, len(to))
	wraps := make([][]carried, len(to))
	for i, pk := range to {
		work := 0
		if kind == "message" && m.Contacts != nil {
			stranger, err := m.stranger(ctx, pk)
			if err != nil {
				return nil, err
			}
			if stranger {
				work = m.work
			}
		}
		seal, entries, err := m.prepare(ctx, pk, rumor, expires, work)
		if err != nil {
			return nil, err
		}
		outs[i] = Outgoing{Kind: kind, Rumor: rumor.ID.Hex(), To: pk.Hex(), Seal: seal.ID.Hex(), Created: m.now().UTC(), Expires: expires.UTC(), Text: rumor.Content}
		wraps[i] = entries
	}
	err := m.update(func(tx kv.Tx) error {
		for i, out := range outs {
			if err := put(tx, outboxBucket, outboxKey(out.Rumor, out.To), out); err != nil {
				return err
			}
			if err := putCarried(tx, wraps[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Transmission is best effort. Durable state survives every failed send.
	for i, pk := range to {
		_, outs[i].Inbox = m.sendPrepared(ctx, pk, wraps[i])
	}
	return outs, nil
}

// prepare persists encrypted events, without exposing anything to a transport.
// Orphaned events are harmless if the following mail transaction fails. With
// work, each form carries NIP-13 proof of work of that many bits: a stick
// carries only the courier form.
func (m *Mail) prepare(ctx context.Context, to nostr.PubKey, rumor nostr.Event, expires time.Time, work int) (nostr.Event, []carried, error) {
	seal, err := private.Seal(ctx, m.key, to, rumor)
	if err != nil {
		return nostr.Event{}, nil, err
	}
	if result, err := m.node.Store.Save(seal); err != nil || result.Outcome == store.Refused {
		return nostr.Event{}, nil, fmt.Errorf("mail: seal not saved: %v %s", err, result.Reason)
	}
	var entries []carried
	for _, form := range []private.Form{private.CourierForm, private.RelayForm} {
		wrap, err := private.WrapSealWork(ctx, seal, to, form, private.WrapKind, expires, work)
		if err != nil {
			return nostr.Event{}, nil, err
		}
		if result, err := m.node.Store.Save(wrap); err != nil || result.Outcome == store.Refused {
			return nostr.Event{}, nil, fmt.Errorf("mail: wrap not saved: %v %s", err, result.Reason)
		}
		entries = append(entries, carried{Wrap: wrap.ID.Hex(), Own: true, Courier: form == private.CourierForm, Hops: StartHops, Received: m.now().UTC(), Expires: expires.UTC(), Rumor: rumor.ID.Hex(), To: to.Hex()})
	}
	return seal, entries, nil
}

func putCarried(tx kv.Tx, entries []carried) error {
	for _, entry := range entries {
		if err := put(tx, carryBucket, entry.Wrap, entry); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mail) sendPrepared(ctx context.Context, to nostr.PubKey, entries []carried) (int, InboxLookup) {
	inbox, lookup, _ := m.inboxRelays(ctx, to)
	messages := 0
	// A message has a wrap in each form. The send of a message is one
	// attempt, however many of its wraps went out.
	counted := map[string]bool{}
	for _, entry := range entries {
		targets := m.relays
		if !entry.Courier {
			targets = slices.Concat(targets, inbox)
		}
		id, _ := nostr.IDFromHex(entry.Wrap)
		events, err := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{id}})
		if err != nil || len(events) == 0 {
			continue
		}
		// Each relay gets the wrap at once, so relays that give no answer
		// cost one wait in all, not one each (#442).
		var todo []transport.Transport
		for _, t := range targets {
			if !slices.Contains(entry.SentTo, t.Name()) && !slices.ContainsFunc(todo, func(o transport.Transport) bool { return o.Name() == t.Name() }) {
				todo = append(todo, t)
			}
		}
		var (
			mu   sync.Mutex
			wg   sync.WaitGroup
			sent bool
		)
		for _, t := range todo {
			wg.Go(func() {
				if t.Send(ctx, events[0]) == nil {
					mu.Lock()
					entry.SentTo = append(entry.SentTo, t.Name())
					sent = true
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		// Failure to record a receipt can only repeat a wrap, never lose a call.
		_ = m.put(carryBucket, entry.Wrap, entry)
		// The outbox counts this send as an attempt, as a sync counts its
		// own. A failure to count it changes only the number that it shows.
		if key := outboxKey(entry.Rumor, entry.To); sent && entry.Own && entry.Rumor != "" && !counted[key] {
			counted[key] = true
			messages++
			_ = m.attempted(key)
		}
	}
	return messages, lookup
}

// Forward sends each pending message, request, acknowledgement and reply
// of this citizen to the relays of the newest relay list of its recipient
// that do not have its wraps yet. A list can appear or change after the
// send. It returns the number of messages sent, one for each message and
// recipient. A delivered message and an expired wrap get nothing.
func (m *Mail) Forward(ctx context.Context) (int, error) {
	var entries []carried
	if err := m.each(func(c carried) {
		if c.Own && c.Rumor != "" && c.To != "" {
			entries = append(entries, c)
		}
	}); err != nil {
		return 0, err
	}
	pending := map[string][]carried{}
	var order []string
	now := m.now()
	for _, entry := range entries {
		var out Outgoing
		found, err := m.get(outboxBucket, outboxKey(entry.Rumor, entry.To), &out)
		if err != nil {
			return 0, err
		}
		// An acknowledgement or a reply has no outbox entry, and waits
		// until it expires.
		if found && !out.Delivered.IsZero() || !now.Before(entry.Expires) {
			continue
		}
		if _, ok := pending[entry.To]; !ok {
			order = append(order, entry.To)
		}
		pending[entry.To] = append(pending[entry.To], entry)
	}
	sent := 0
	for _, to := range order {
		pk, err := nostr.PubKeyFromHex(to)
		if err != nil {
			continue
		}
		n, _ := m.sendPrepared(ctx, pk, pending[to])
		sent += n
	}
	return sent, nil
}

// post durably queues an acknowledgement or reply before transmitting it.
func (m *Mail) post(ctx context.Context, to nostr.PubKey, rumor nostr.Event, expires time.Time) (nostr.Event, error) {
	seal, entries, err := m.prepare(ctx, to, rumor, expires, 0)
	if err != nil {
		return nostr.Event{}, err
	}
	if err := m.update(func(tx kv.Tx) error { return putCarried(tx, entries) }); err != nil {
		return nostr.Event{}, err
	}
	m.sendPrepared(ctx, to, entries)
	return seal, nil
}

// RelayListKind is the kind of the list of relays where a citizen reads its
// direct messages, as NIP-17 defines.
const RelayListKind nostr.Kind = 10050

// RelayList makes the list of relays where this citizen reads its mail.
func RelayList(ctx context.Context, k keys.Signer, urls []string, at nostr.Timestamp) (nostr.Event, error) {
	tags := nostr.Tags{}
	for _, url := range urls {
		tags = append(tags, nostr.Tag{"relay", url})
	}
	event := nostr.Event{Kind: RelayListKind, CreatedAt: at, Tags: tags}
	if err := k.SignEvent(ctx, &event); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// inboxRelays returns the relays where a citizen reads its mail, from its
// newest NIP-17 relay list. Each lookup asks the relays and the indexers of
// this citizen first, for at most LookupWait, because the list in the store
// can be old. If none of them answers, it uses the list in the store.
func (m *Mail) inboxRelays(ctx context.Context, to nostr.PubKey) ([]transport.Transport, InboxLookup, error) {
	if m.InboxRelay == nil {
		return nil, InboxNone, nil
	}
	filter := nostr.Filter{Kinds: []nostr.Kind{RelayListKind}, Authors: []nostr.PubKey{to}}
	answered := false
	if via := slices.Concat(m.relays, m.Indexers); len(via) > 0 {
		lookup, cancel := context.WithTimeout(ctx, LookupWait)
		reports, _ := m.node.Pull(lookup, filter, via)
		cancel()
		answered = len(reports) > 0
	}
	lists, err := m.node.Store.Query(filter)
	if err != nil {
		return nil, InboxNone, err
	}
	if len(lists) == 0 {
		return nil, InboxNone, nil
	}
	// The list is replaceable, so the store keeps only the newest.
	var out []transport.Transport
	for _, tag := range lists[0].Tags {
		if len(tag) > 1 && tag[0] == "relay" {
			out = append(out, m.InboxRelay(tag[1]))
		}
	}
	if !answered {
		return out, InboxStored, nil
	}
	return out, InboxFound, nil
}
