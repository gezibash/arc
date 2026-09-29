// Package mail moves private events between citizens, even when no path
// exists between them now.
//
// A message is a NIP-17 rumor of kind 14, sealed by its author and wrapped
// twice: once in the relay form, for relays, and once in the courier form,
// for anything that a person carries. Both wraps hold the same seal, so a
// recipient who gets both keeps one.
//
// The outbox keeps each message until the recipient acknowledges it. A node
// that syncs with a directory also carries the courier form of other
// citizens' mail, which it cannot read: it knows only a route tag. A hop limit
// beside each event bounds how many couriers carry it on.
package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/journal"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// The kinds and limits of mail.
const (
	MessageKind nostr.Kind = 14
	AckKind     nostr.Kind = 3274

	// StartHops is the number of couriers that may carry a new message.
	StartHops = 3
	// MaxCarried caps the mail of other citizens that one node carries.
	MaxCarried = 40
	// MaxCarryBytes caps one carried event.
	MaxCarryBytes = 64 * 1024
)

var (
	outboxBucket = []byte("outbox")
	carryBucket  = []byte("carry")
	inboxBucket  = []byte("requests")
)

// Outgoing is one message or call in the outbox.
type Outgoing struct {
	// Kind is "message", or "request" for a store-and-forward call.
	Kind      string    `json:"kind"`
	Rumor     string    `json:"rumor"`
	To        string    `json:"to"`
	Seal      string    `json:"seal"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	Attempts  int       `json:"attempts"`
	Delivered time.Time `json:"delivered,omitzero"`
	// ReplySeal names the seal of the reply to a request.
	ReplySeal string `json:"reply_seal,omitempty"`

	// Text is the message, opened from the sender's own seal, and Reply is
	// the reply to a request, opened from its seal. Neither is stored.
	Text  string     `json:"-"`
	Reply call.Reply `json:"-"`
}

// State says where a message is.
func (o Outgoing) State(now time.Time) string {
	switch {
	case !o.Delivered.IsZero():
		return "delivered"
	case now.After(o.Expires):
		return "expired"
	default:
		return "pending"
	}
}

// carried is one wrap that this node moves: its own mail, or another
// citizen's.
type carried struct {
	Wrap     string    `json:"wrap"`
	Own      bool      `json:"own"`
	Courier  bool      `json:"courier"`
	Hops     int       `json:"hops"`
	Received time.Time `json:"received"`
	Expires  time.Time `json:"expires"`
	Rumor    string    `json:"rumor,omitempty"`
	SentTo   []string  `json:"sent_to,omitempty"`
}

// Mail is the mail of one citizen on one machine.
type Mail struct {
	key    keys.Signer
	node   *node.Node
	db     journal.Store
	relays []transport.Transport
	now    func() time.Time

	// Indexers are relays that hold relay lists. Mail looks up the NIP-17
	// list of a recipient there when its own relays do not hold it.
	Indexers []transport.Transport
	// InboxRelay constructs a transport for a discovered NIP-17 inbox URL.
	// The application supplies its relay adapter and anonymous authentication.
	InboxRelay func(string) transport.Transport

	// OnRequest answers a store-and-forward call to this citizen. A citizen
	// that serves no capability leaves it nil; requests remain pending until a handler is attached.
	OnRequest func(ctx context.Context, rumor nostr.Event) (call.Reply, error)
}

// New initializes durable mail using an injected transactional journal.
// The relays are where new mail is sent at once.
func New(db journal.Store, k keys.Signer, n *node.Node, relays []transport.Transport) (*Mail, error) {
	m := &Mail{key: k, node: n, db: db, relays: relays, now: time.Now}
	if err := m.update(func(tx journal.Tx) error {
		for _, name := range [][]byte{outboxBucket, carryBucket, inboxBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("mail: %w", err)
	}
	return m, nil
}

// Close releases the mail state.
func (m *Mail) Close() error                           { m.db.Close(); return nil }
func (m *Mail) update(fn func(journal.Tx) error) error { return m.db.Update(fn) }
func (m *Mail) view(fn func(journal.Tx) error) error   { return m.db.View(fn) }

// Send seals a message to a citizen, puts it in the outbox, and sends it to
// the relays at once. A relay that fails does not fail the send: the outbox
// sends the message again on the next sync.
func (m *Mail) Send(ctx context.Context, to nostr.PubKey, text string) (Outgoing, error) {
	return m.SendRumor(ctx, to, MessageKind, text, nostr.Tags{{"p", to.Hex()}})
}

// SendRumor seals a private event of any kind to a citizen, as Send does
// with a message.
func (m *Mail) SendRumor(ctx context.Context, to nostr.PubKey, kind nostr.Kind, content string, tags nostr.Tags) (Outgoing, error) {
	if to == m.key.PublicKey() {
		return Outgoing{}, errors.New("mail: a message to yourself belongs in the journal")
	}
	switch kind {
	case AckKind, call.RequestKind, call.ReplyKind:
		return Outgoing{}, fmt.Errorf("mail: kind %d belongs to the mail layer", kind)
	}

	now := m.now()
	rumor := private.Rumor(m.key, kind, content, tags, now)
	return m.queue(ctx, to, rumor, "message")
}

// Request commits a call to the outbox before attempting any network send.
func (m *Mail) Request(ctx context.Context, provider nostr.PubKey, r call.Request) (Outgoing, error) {
	r.DeadlineMS = 0 // A deferred request's execution budget starts when served.
	return m.queue(ctx, provider, call.RequestRumor(m.key, provider, r, m.now()), "request")
}

func (m *Mail) queue(ctx context.Context, to nostr.PubKey, rumor nostr.Event, kind string) (Outgoing, error) {
	expires := m.now().Add(private.MaxAge)
	seal, entries, err := m.prepare(ctx, to, rumor, expires)
	if err != nil {
		return Outgoing{}, err
	}
	out := Outgoing{Kind: kind, Rumor: rumor.ID.Hex(), To: to.Hex(), Seal: seal.ID.Hex(), Created: m.now().UTC(), Expires: expires.UTC(), Text: rumor.Content}
	err = m.update(func(tx journal.Tx) error {
		if err := put(tx, outboxBucket, out.Rumor, out); err != nil {
			return err
		}
		return putCarried(tx, entries)
	})
	if err != nil {
		return Outgoing{}, err
	}
	// Transmission is best effort. Durable state survives every failed send.
	m.sendPrepared(ctx, to, entries)
	return out, nil
}

// prepare persists encrypted events, without exposing anything to a transport.
// Orphaned events are harmless if the following mail transaction fails.
func (m *Mail) prepare(ctx context.Context, to nostr.PubKey, rumor nostr.Event, expires time.Time) (nostr.Event, []carried, error) {
	seal, err := private.Seal(ctx, m.key, to, rumor)
	if err != nil {
		return nostr.Event{}, nil, err
	}
	if result, err := m.node.Store.Save(seal); err != nil || result.Outcome == store.Refused {
		return nostr.Event{}, nil, fmt.Errorf("mail: seal not saved: %v %s", err, result.Reason)
	}
	var entries []carried
	for _, form := range []private.Form{private.CourierForm, private.RelayForm} {
		wrap, err := private.WrapSeal(seal, to, form, private.WrapKind, expires)
		if err != nil {
			return nostr.Event{}, nil, err
		}
		if result, err := m.node.Store.Save(wrap); err != nil || result.Outcome == store.Refused {
			return nostr.Event{}, nil, fmt.Errorf("mail: wrap not saved: %v %s", err, result.Reason)
		}
		entries = append(entries, carried{Wrap: wrap.ID.Hex(), Own: true, Courier: form == private.CourierForm, Hops: StartHops, Received: m.now().UTC(), Expires: expires.UTC(), Rumor: rumor.ID.Hex()})
	}
	return seal, entries, nil
}

func putCarried(tx journal.Tx, entries []carried) error {
	for _, entry := range entries {
		if err := put(tx, carryBucket, entry.Wrap, entry); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mail) sendPrepared(ctx context.Context, to nostr.PubKey, entries []carried) {
	inbox, _ := m.inboxRelays(ctx, to)
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
		for _, t := range targets {
			if !slices.Contains(entry.SentTo, t.Name()) && t.Send(ctx, events[0]) == nil {
				entry.SentTo = append(entry.SentTo, t.Name())
			}
		}
		// Failure to record a receipt can only repeat a wrap, never lose a call.
		_ = m.put(carryBucket, entry.Wrap, entry)
	}
}

// post durably queues an acknowledgement or reply before transmitting it.
func (m *Mail) post(ctx context.Context, to nostr.PubKey, rumor nostr.Event, expires time.Time) (nostr.Event, error) {
	seal, entries, err := m.prepare(ctx, to, rumor, expires)
	if err != nil {
		return nostr.Event{}, err
	}
	if err := m.update(func(tx journal.Tx) error { return putCarried(tx, entries) }); err != nil {
		return nostr.Event{}, err
	}
	m.sendPrepared(ctx, to, entries)
	return seal, nil
}

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
	if err := m.expire(); err != nil {
		return report, err
	}
	if err := m.pending(ctx, &report); err != nil {
		return report, err
	}

	carrier, isCarrier := t.(transport.Carrier)
	mine := private.RouteTags(m.key.PublicKey(), m.now())

	var filters []nostr.Filter
	if isCarrier {
		filters = []nostr.Filter{{Kinds: []nostr.Kind{private.WrapKind}}}
	} else {
		filters = []nostr.Filter{
			{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {m.key.PublicKey().Hex()}}},
			{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"w": mine}},
		}
	}

	for _, filter := range filters {
		batch, err := t.Fetch(ctx, filter)
		if err != nil {
			return report, err
		}
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

// incoming separates receipt from execution. Receipt holds the encrypted seal
// until the event store has it; committing this journal always comes first.
// A stale processing record is uncertain, never permission to repeat a mutation.
type incoming struct {
	Seal      string       `json:"seal"`
	Receipt   *nostr.Event `json:"receipt,omitempty"`
	State     string       `json:"state"`
	Started   time.Time    `json:"started,omitzero"`
	ReplySeal string       `json:"reply_seal,omitempty"`
}

func (m *Mail) receiveRequest(ctx context.Context, opened private.Opened, previouslyReceived bool, report *Report) error {
	id := opened.Rumor.ID.Hex()
	err := m.update(func(tx journal.Tx) error {
		if tx.Bucket(inboxBucket).Get([]byte(id)) != nil {
			return nil
		}
		entry := incoming{Seal: opened.Seal.ID.Hex(), Receipt: &opened.Seal, State: "pending"}
		// A seal written by an older ARC version has no execution journal. It
		// might already have run; do not execute it again during an upgrade.
		if previouslyReceived {
			entry.State = "processing"
		}
		return put(tx, inboxBucket, id, entry)
	})
	if err != nil {
		return err
	}
	return m.answer(ctx, id, report)
}

func (m *Mail) pending(ctx context.Context, report *Report) error {
	var ids []string
	if err := m.view(func(tx journal.Tx) error {
		return tx.Bucket(inboxBucket).ForEach(func(k, v []byte) error {
			var entry incoming
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			if entry.State != "completed" {
				ids = append(ids, string(k))
			}
			return nil
		})
	}); err != nil {
		return err
	}
	for _, id := range ids {
		if err := m.answer(ctx, id, report); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mail) answer(ctx context.Context, id string, report *Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var entry incoming
	found, err := m.get(inboxBucket, id, &entry)
	if err != nil || !found || entry.State == "completed" {
		return err
	}
	if entry.Receipt != nil {
		// A crash on either side of Save leaves enough ciphertext in the
		// journal to finish receipt without waiting for another delivery.
		result, err := m.node.Store.Save(*entry.Receipt)
		if err != nil {
			return err
		}
		err = m.update(func(tx journal.Tx) error {
			entry = incoming{}
			if err := json.Unmarshal(tx.Bucket(inboxBucket).Get([]byte(id)), &entry); err != nil {
				return err
			}
			if entry.Receipt == nil {
				return nil
			}
			entry.Receipt = nil
			if result.Outcome == store.Refused {
				entry.State = "completed"
			}
			return put(tx, inboxBucket, id, entry)
		})
		if err != nil {
			return err
		}
		if result.Outcome == store.Refused {
			report.Refused = append(report.Refused, id+": "+result.Reason)
			return nil
		}
	}
	if m.OnRequest == nil || entry.State == "completed" {
		return nil
	}
	if entry.State == "processing" && !entry.Started.IsZero() && m.now().Before(entry.Started.Add(call.Timeout+time.Minute)) {
		return nil
	}
	// Opening the request cannot execute it. A failed read or signer call
	// must leave pending work retryable. Claim it atomically only afterward.
	sealID, err := nostr.IDFromHex(entry.Seal)
	if err != nil {
		return err
	}
	seals, err := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{sealID}})
	if err != nil {
		return err
	}
	if len(seals) == 0 {
		return fmt.Errorf("mail: request %s has no saved seal", id)
	}
	request, err := private.OpenSeal(ctx, m.key, seals[0])
	if err != nil {
		return err
	}
	var run, uncertain bool
	err = m.update(func(tx journal.Tx) error {
		data := tx.Bucket(inboxBucket).Get([]byte(id))
		if data == nil {
			return nil
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		switch entry.State {
		case "completed":
			return nil
		case "pending":
			run = true
		case "processing":
			if !entry.Started.IsZero() && m.now().Before(entry.Started.Add(call.Timeout+time.Minute)) {
				return nil
			}
			uncertain = true
		default:
			return fmt.Errorf("mail: unknown request state %q", entry.State)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entry.State = "processing"
		entry.Started = m.now().UTC()
		return put(tx, inboxBucket, id, entry)
	})
	if err != nil || !run && !uncertain {
		return err
	}
	reply := call.Reply{Err: "outcome_unknown a previous execution did not record a result"}
	if run {
		budget, cancel := context.WithTimeout(ctx, call.Timeout)
		reply, err = m.OnRequest(budget, request)
		cancel()
		if err != nil {
			reply = call.Reply{Err: err.Error()}
		}
	}
	rumor := call.ReplyRumor(m.key, request, reply, m.now())
	seal, entries, err := m.prepare(ctx, request.PubKey, rumor, m.now().Add(private.MaxAge))
	if err != nil {
		return err
	}
	entry.State = "completed"
	entry.ReplySeal = seal.ID.Hex()
	if err := m.update(func(tx journal.Tx) error {
		if err := putCarried(tx, entries); err != nil {
			return err
		}
		return put(tx, inboxBucket, id, entry)
	}); err != nil {
		return err
	}
	m.sendPrepared(ctx, request.PubKey, entries)
	report.Answered++
	return nil
}

// replied files the reply to one of this citizen's calls, if the provider
// that the call went to sent it.
func (m *Mail) replied(opened private.Opened, report *Report) error {
	id, _ := call.ReadReply(opened.Rumor)

	var out Outgoing
	found, err := m.get(outboxBucket, id, &out)
	if err != nil || !found {
		return err
	}
	if out.Kind != "request" || out.To != opened.Author().Hex() || out.ReplySeal != "" {
		return nil
	}

	out.Delivered = m.now().UTC()
	out.ReplySeal = opened.Seal.ID.Hex()
	report.Replies++
	return m.put(outboxBucket, out.Rumor, out)
}

// acknowledged marks a message delivered, if its recipient sent the
// acknowledgement.
func (m *Mail) acknowledged(opened private.Opened, report *Report) error {
	tag := opened.Rumor.Tags.Find("e")
	if len(tag) < 2 {
		return nil
	}

	var out Outgoing
	found, err := m.get(outboxBucket, tag[1], &out)
	if err != nil || !found {
		return err
	}
	if out.To != opened.Author().Hex() || !out.Delivered.IsZero() {
		return nil
	}

	out.Delivered = m.now().UTC()
	report.Delivered++
	return m.put(outboxBucket, out.Rumor, out)
}

// carry takes on another citizen's courier-form wrap, if the hop limit
// allows. The limit comes from beside the event on the carrier, and a reader
// never trusts it past StartHops.
func (m *Mail) carry(wrap nostr.Event, hops int, report *Report) error {
	if hops > StartHops {
		hops = StartHops
	}
	if hops <= 0 || wrap.Tags.Find("w") == nil {
		return nil
	}
	if body, _ := json.Marshal(wrap); len(body) > MaxCarryBytes {
		return nil
	}

	var held carried
	if found, err := m.get(carryBucket, wrap.ID.Hex(), &held); err != nil || found {
		return err
	}

	result, err := m.node.Store.Save(wrap)
	if err != nil {
		return err
	}
	if result.Outcome == store.Refused {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": "+result.Reason)
		return nil
	}

	entry := carried{
		Wrap: wrap.ID.Hex(), Courier: true, Hops: hops - 1,
		Received: m.now().UTC(), Expires: expiry(wrap, m.now()),
	}
	if err := m.put(carryBucket, entry.Wrap, entry); err != nil {
		return err
	}
	report.Carried++
	return nil
}

// expiry is the expiration of a wrap, or MaxAge from now when it names none.
func expiry(wrap nostr.Event, now time.Time) time.Time {
	if tag := wrap.Tags.Find("expiration"); len(tag) > 1 {
		if at, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
			return time.Unix(at, 0).UTC()
		}
	}
	return now.Add(private.MaxAge).UTC()
}

// evict drops the oldest mail of other citizens past MaxCarried.
func (m *Mail) evict() error {
	var others []carried
	if err := m.each(func(c carried) {
		if !c.Own {
			others = append(others, c)
		}
	}); err != nil {
		return err
	}
	if len(others) <= MaxCarried {
		return nil
	}

	sort.Slice(others, func(a, b int) bool { return others[a].Received.Before(others[b].Received) })
	return m.update(func(tx journal.Tx) error {
		for _, c := range others[:len(others)-MaxCarried] {
			if err := tx.Bucket(carryBucket).Delete([]byte(c.Wrap)); err != nil {
				return err
			}
		}
		return nil
	})
}

// give sends the transport every wrap that this node moves. A carrier gets
// the courier form only, with its hop limit, because the relay form names the
// recipient. A relay gets each wrap once.
func (m *Mail) give(ctx context.Context, t transport.Transport, carrier transport.Carrier, isCarrier bool, report *Report) error {
	var entries []carried
	if err := m.each(func(c carried) { entries = append(entries, c) }); err != nil {
		return err
	}

	for _, entry := range entries {
		if isCarrier && !entry.Courier {
			continue
		}
		if !isCarrier && slices.Contains(entry.SentTo, t.Name()) {
			continue
		}

		id, err := nostr.IDFromHex(entry.Wrap)
		if err != nil {
			continue
		}
		events, err := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{id}})
		if err != nil {
			return err
		}
		if len(events) == 0 {
			continue
		}

		if isCarrier {
			err = carrier.SendHops(ctx, events[0], entry.Hops)
		} else {
			err = t.Send(ctx, events[0])
		}
		if err != nil {
			report.Refused = append(report.Refused, "not sent "+entry.Wrap+": "+err.Error())
			continue
		}
		report.Sent++

		if !isCarrier {
			entry.SentTo = append(entry.SentTo, t.Name())
			if err := m.put(carryBucket, entry.Wrap, entry); err != nil {
				return err
			}
		}
		if entry.Own && entry.Rumor != "" {
			if err := m.attempted(entry.Rumor); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Mail) attempted(rumor string) error {
	return m.update(func(tx journal.Tx) error {
		data := tx.Bucket(outboxBucket).Get([]byte(rumor))
		if data == nil {
			return nil
		}
		var out Outgoing
		if err := json.Unmarshal(data, &out); err != nil {
			return err
		}
		if !out.Delivered.IsZero() {
			return nil
		}
		out.Attempts++
		return put(tx, outboxBucket, rumor, out)
	})
}

func (m *Mail) expire() error {
	return m.update(func(tx journal.Tx) error {
		cursor := tx.Bucket(carryBucket).Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var entry carried
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			if m.now().After(entry.Expires) {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Message is one message that this citizen received.
type Message struct {
	ID   string
	From nostr.PubKey
	At   time.Time
	Text string
}

// Inbox returns the messages that this citizen received, oldest first. It
// opens each seal from the store, and stores no text.
func (m *Mail) Inbox(ctx context.Context) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Message
	seen := map[string]bool{}

	seals, err := m.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.SealKind}})
	if err != nil {
		return nil, err
	}
	for _, seal := range seals {
		if seal.PubKey == m.key.PublicKey() {
			continue
		}
		rumor, err := private.OpenSeal(ctx, m.key, seal)
		if err != nil {
			return nil, fmt.Errorf("mail: open received seal %s: %w", seal.ID.Hex(), err)
		}
		if rumor.Kind != MessageKind || seen[rumor.ID.Hex()] {
			continue
		}
		seen[rumor.ID.Hex()] = true
		out = append(out, Message{
			ID: rumor.ID.Hex(), From: rumor.PubKey,
			At: time.Unix(int64(rumor.CreatedAt), 0).UTC(), Text: rumor.Content,
		})
	}

	sort.Slice(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out, nil
}

// Rumors returns the private events of some kinds that this citizen
// received and sent, oldest first. It opens each seal from the store.
func (m *Mail) Rumors(ctx context.Context, kinds []nostr.Kind) ([]nostr.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []nostr.Event
	seen := map[nostr.ID]bool{}
	keep := func(rumor nostr.Event) {
		if !slices.Contains(kinds, rumor.Kind) || seen[rumor.ID] {
			return
		}
		seen[rumor.ID] = true
		out = append(out, rumor)
	}

	seals, err := m.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.SealKind}})
	if err != nil {
		return nil, err
	}
	for _, seal := range seals {
		if seal.PubKey == m.key.PublicKey() {
			continue
		}
		rumor, err := private.OpenSeal(ctx, m.key, seal)
		if err != nil {
			return nil, fmt.Errorf("mail: open received seal %s: %w", seal.ID.Hex(), err)
		}
		keep(rumor)
	}
	outgoing, err := m.outgoing()
	if err != nil {
		return nil, err
	}
	for _, o := range outgoing {
		rumor, err := m.ownRumor(ctx, o)
		if err != nil {
			return nil, err
		}
		keep(rumor)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt < out[b].CreatedAt })
	return out, nil
}

func (m *Mail) outgoing() ([]Outgoing, error) {
	var out []Outgoing
	err := m.view(func(tx journal.Tx) error {
		return tx.Bucket(outboxBucket).ForEach(func(_, v []byte) error {
			var o Outgoing
			if err := json.Unmarshal(v, &o); err != nil {
				return err
			}
			out = append(out, o)
			return nil
		})
	})
	return out, err
}

// Outbox returns sent messages and their replies, oldest first.
func (m *Mail) Outbox(ctx context.Context) ([]Outgoing, error) {
	out, err := m.outgoing()
	if err != nil {
		return nil, err
	}
	for i := range out {
		rumor, err := m.ownRumor(ctx, out[i])
		if err != nil {
			return nil, err
		}
		out[i].Text = rumor.Content
		out[i].Reply, err = m.replyOf(ctx, out[i])
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.Before(out[b].Created) })
	return out, nil
}

func (m *Mail) savedSeal(text string) (nostr.Event, error) {
	id, err := nostr.IDFromHex(text)
	if err != nil {
		return nostr.Event{}, err
	}
	seals, err := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{id}})
	if err != nil {
		return nostr.Event{}, err
	}
	if len(seals) == 0 {
		return nostr.Event{}, fmt.Errorf("mail: missing seal %s", text)
	}
	return seals[0], nil
}
func (m *Mail) ownRumor(ctx context.Context, o Outgoing) (nostr.Event, error) {
	to, err := nostr.PubKeyFromHex(o.To)
	if err != nil {
		return nostr.Event{}, err
	}
	seal, err := m.savedSeal(o.Seal)
	if err != nil {
		return nostr.Event{}, err
	}
	return private.OpenOwnSeal(ctx, m.key, seal, to)
}
func (m *Mail) replyOf(ctx context.Context, o Outgoing) (call.Reply, error) {
	if o.ReplySeal == "" {
		return call.Reply{}, nil
	}
	seal, err := m.savedSeal(o.ReplySeal)
	if err != nil {
		return call.Reply{}, err
	}
	rumor, err := private.OpenSeal(ctx, m.key, seal)
	if err != nil {
		return call.Reply{}, err
	}
	_, reply := call.ReadReply(rumor)
	return reply, nil
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
// NIP-17 relay list. It looks in the store first, then asks the relays.
func (m *Mail) inboxRelays(ctx context.Context, to nostr.PubKey) ([]transport.Transport, error) {
	if m.InboxRelay == nil {
		return nil, nil
	}
	filter := nostr.Filter{Kinds: []nostr.Kind{RelayListKind}, Authors: []nostr.PubKey{to}}
	lists, err := m.node.Store.Query(filter)
	if err != nil {
		return nil, err
	}
	if via := slices.Concat(m.relays, m.Indexers); len(lists) == 0 && len(via) > 0 {
		m.node.Pull(ctx, filter, via)
		lists, err = m.node.Store.Query(filter)
		if err != nil {
			return nil, err
		}
	}
	if len(lists) == 0 {
		return nil, nil
	}
	var out []transport.Transport
	for _, tag := range lists[0].Tags {
		if len(tag) > 1 && tag[0] == "relay" {
			out = append(out, m.InboxRelay(tag[1]))
		}
	}
	return out, nil
}

func (m *Mail) Carrying() (int, error) {
	count := 0
	err := m.each(func(c carried) {
		if !c.Own {
			count++
		}
	})
	return count, err
}
func (m *Mail) each(fn func(carried)) error {
	return m.view(func(tx journal.Tx) error {
		return tx.Bucket(carryBucket).ForEach(func(_, v []byte) error {
			var c carried
			if err := json.Unmarshal(v, &c); err != nil {
				return err
			}
			fn(c)
			return nil
		})
	})
}
func put(tx journal.Tx, bucket []byte, key string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), body)
}
func (m *Mail) put(bucket []byte, key string, value any) error {
	return m.update(func(tx journal.Tx) error { return put(tx, bucket, key, value) })
}

func (m *Mail) get(bucket []byte, key string, into any) (bool, error) {
	var body []byte
	err := m.view(func(tx journal.Tx) error {
		if v := tx.Bucket(bucket).Get([]byte(key)); v != nil {
			body = append([]byte(nil), v...)
		}
		return nil
	})
	if err != nil || body == nil {
		return false, err
	}
	return true, json.Unmarshal(body, into)
}
