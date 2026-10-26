// Package mail moves private events between citizens, even when no path
// exists between them now.
//
// A message is a NIP-17 rumor of kind 14, sealed by its author and wrapped
// twice: once in the relay form, for relays, and once in the courier form,
// for anything that a person carries. Both wraps hold the same seal, so a
// recipient who gets both keeps one.
//
// A group message is one rumor with a p tag for each member. The author
// seals it to each member separately, and the outbox keeps one entry for
// each member, so each member acknowledges it separately.
//
// The outbox keeps each message until the recipient acknowledges it. A node
// that syncs with a directory also carries the courier form of other
// citizens' mail, which it cannot read: it knows only a route tag. A hop limit
// beside each event bounds how many couriers carry it on.
//
// A sync fetches the mail from a transport. A watch keeps a subscription on
// a live transport instead, and takes each wrap as it arrives.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
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
	// MaxParticipants caps the members of a group message, with its
	// author, as NIP-17 advises: each member costs two wraps and one
	// acknowledgement.
	MaxParticipants = 10
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
	// ReplyTo is the parent of a message that replies to one, from the
	// sender's own seal. It is not stored.
	ReplyTo string `json:"-"`
	// Inbox says how the send found the relay list of the recipient. It is
	// not stored.
	Inbox InboxLookup `json:"-"`
}

// InboxLookup is the result of the lookup of the NIP-17 relay list of a
// recipient.
type InboxLookup string

const (
	// InboxFound says that a relay or an indexer answered the lookup, and
	// the newest list was used.
	InboxFound InboxLookup = "found"
	// InboxStored says that no relay or indexer answered, and the send used
	// the list in the store, which can be old.
	InboxStored InboxLookup = "stored"
	// InboxNone says that no list was found: the mail waits on the relays
	// of the sender only.
	InboxNone InboxLookup = "none"
)

// LookupWait bounds the lookup of the relay list of a recipient.
const LookupWait = 5 * time.Second

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
	// To is the recipient of the wrap. With Rumor, it names the outbox
	// entry.
	To     string   `json:"to,omitempty"`
	SentTo []string `json:"sent_to,omitempty"`
}

// outboxKey names the outbox entry of a rumor to one recipient. A group
// message has one entry for each member.
func outboxKey(rumor, to string) string { return rumor + ":" + to }

// Mail is the mail of one citizen on one machine.
type Mail struct {
	key    keys.Signer
	node   *node.Node
	db     kv.Store
	relays []transport.Transport
	now    func() time.Time
	// receiving serializes the work on received mail. Syncs and watches of
	// several transports can take the same wrap at the same time.
	receiving sync.Mutex

	// Indexers are relays that hold relay lists. Mail looks up the NIP-17
	// list of a recipient there when its own relays do not hold it.
	Indexers []transport.Transport
	// Unanswered says whether an error of a send means that the transport
	// gave no answer, so that the sends after it would wait in vain. A sync
	// then stops sending to that transport (#465). Nil keeps each send.
	Unanswered func(error) bool
	// InboxRelay constructs a transport for a discovered NIP-17 inbox URL.
	// The application supplies its relay adapter and anonymous authentication.
	InboxRelay func(string) transport.Transport

	// OnRequest answers a store-and-forward call to this citizen. A citizen
	// that serves no capability leaves it nil; requests remain pending until a handler is attached.
	OnRequest func(ctx context.Context, rumor nostr.Event) (call.Reply, error)
	// Serves says whether OnRequest serves a request. A request that it
	// does not serve remains pending, for another program of this citizen
	// that serves its capability. Nil serves each request.
	Serves func(rumor nostr.Event) bool

	// Contacts is the contact list of this citizen. With it, mail from a
	// stranger reaches the inbox only with proof of work, and a message to
	// a recipient that never wrote to this citizen carries work. Nil takes
	// all mail, and makes no work.
	Contacts ContactList
	// work is the difficulty that admits mail from a stranger, and that a
	// sender makes: private.Work.
	work int
	// maxHeld caps the held mail: MaxHeld.
	maxHeld int
}

// New initializes durable mail using an injected transactional key-value store.
// The relays are where new mail is sent at once.
func New(db kv.Store, k keys.Signer, n *node.Node, relays []transport.Transport) (*Mail, error) {
	m := &Mail{key: k, node: n, db: db, relays: relays, now: time.Now, work: private.Work, maxHeld: MaxHeld}
	if err := m.update(func(tx kv.Tx) error {
		for _, name := range [][]byte{outboxBucket, carryBucket, inboxBucket, heldBucket, droppedBucket, wroteBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return migrateOutbox(tx)
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("mail: %w", err)
	}
	return m, nil
}

// migrateOutbox moves each outbox entry of an older arc, which the rumor id
// alone names, to the key of its rumor and its recipient. It also names the
// recipient of each own wrap of those entries. It runs in the transaction
// that opens the mail, so a failure leaves the older layout whole.
func migrateOutbox(tx kv.Tx) error {
	outbox := tx.Bucket(outboxBucket)
	moved := map[string]string{}
	if err := outbox.ForEach(func(k, v []byte) error {
		if len(k) != 64 {
			return nil
		}
		var out Outgoing
		if err := json.Unmarshal(v, &out); err != nil {
			return err
		}
		moved[string(k)] = out.To
		return nil
	}); err != nil || len(moved) == 0 {
		return err
	}
	for rumor, to := range moved {
		body := append([]byte(nil), outbox.Get([]byte(rumor))...)
		if err := outbox.Put([]byte(outboxKey(rumor, to)), body); err != nil {
			return err
		}
		if err := outbox.Delete([]byte(rumor)); err != nil {
			return err
		}
	}
	carry := tx.Bucket(carryBucket)
	var named []carried
	if err := carry.ForEach(func(_, v []byte) error {
		var c carried
		if err := json.Unmarshal(v, &c); err != nil {
			return err
		}
		if to, ok := moved[c.Rumor]; ok && c.Own && c.To == "" {
			c.To = to
			named = append(named, c)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, c := range named {
		if err := put(tx, carryBucket, c.Wrap, c); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the mail state.
func (m *Mail) Close() error                      { m.db.Close(); return nil }
func (m *Mail) update(fn func(kv.Tx) error) error { return m.db.Update(fn) }
func (m *Mail) view(fn func(kv.Tx) error) error   { return m.db.View(fn) }

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
	err := m.update(func(tx kv.Tx) error {
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
	if err := m.view(func(tx kv.Tx) error {
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
		err = m.update(func(tx kv.Tx) error {
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
	if m.Serves != nil && !m.Serves(request) {
		return nil
	}
	var run, uncertain bool
	err = m.update(func(tx kv.Tx) error {
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
	if err := private.CheckMail(rumor); err != nil {
		// The caller gets an error in place of a reply that mail cannot carry.
		rumor = call.ReplyRumor(m.key, request, call.Reply{Err: TooLargeReply}, m.now())
	}
	seal, entries, err := m.prepare(ctx, request.PubKey, rumor, m.now().Add(private.MaxAge), 0)
	if err != nil {
		return err
	}
	entry.State = "completed"
	entry.ReplySeal = seal.ID.Hex()
	if err := m.update(func(tx kv.Tx) error {
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

	key := outboxKey(id, opened.Author().Hex())
	var out Outgoing
	found, err := m.get(outboxBucket, key, &out)
	if err != nil || !found {
		return err
	}
	if out.Kind != "request" || out.ReplySeal != "" {
		return nil
	}

	out.Delivered = m.now().UTC()
	out.ReplySeal = opened.Seal.ID.Hex()
	report.Replies++
	return m.put(outboxBucket, key, out)
}

// acknowledged marks a message delivered to the author of the
// acknowledgement. The entries of the other members of a group message
// stay as they are.
func (m *Mail) acknowledged(opened private.Opened, report *Report) error {
	tag := opened.Rumor.Tags.Find("e")
	if len(tag) < 2 {
		return nil
	}

	key := outboxKey(tag[1], opened.Author().Hex())
	var out Outgoing
	found, err := m.get(outboxBucket, key, &out)
	if err != nil || !found {
		return err
	}
	if !out.Delivered.IsZero() {
		return nil
	}

	out.Delivered = m.now().UTC()
	report.Delivered++
	return m.put(outboxBucket, key, out)
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
	return m.update(func(tx kv.Tx) error {
		for _, c := range others[:len(others)-MaxCarried] {
			if err := tx.Bucket(carryBucket).Delete([]byte(c.Wrap)); err != nil {
				return err
			}
		}
		return nil
	})
}

// StoppedError says that a transport stopped answering partway through the
// sends of a sync. The wraps that did not go wait for the next sync.
type StoppedError struct {
	// Transport is the name of the transport.
	Transport string
	// Sent is the number of wraps that it took, of Waiting.
	Sent, Waiting int
	// Err is the error of the send that got no answer.
	Err error
}

func (e *StoppedError) Error() string {
	return fmt.Sprintf("%s stopped answering after it took %d of %d wraps: %v", e.Transport, e.Sent, e.Waiting, e.Err)
}

func (e *StoppedError) Unwrap() error { return e.Err }

// give sends the transport every wrap that this node moves. A carrier gets
// the courier form only, with its hop limit, because the relay form names the
// recipient. A relay gets each wrap once.
func (m *Mail) give(ctx context.Context, t transport.Transport, carrier transport.Carrier, isCarrier bool, report *Report) error {
	var entries []carried
	if err := m.each(func(c carried) { entries = append(entries, c) }); err != nil {
		return err
	}

	waiting := 0
	for _, entry := range entries {
		if !isCarrier && !slices.Contains(entry.SentTo, t.Name()) {
			waiting++
		}
	}
	sent := 0
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
		// A relay that gave no answer would make each wrap after this one
		// wait or fail the same way: stop, and keep the rest for the next
		// sync. It refused nothing, so no wrap counts as refused (#465).
		if err != nil && !isCarrier && m.Unanswered != nil && m.Unanswered(err) {
			return &StoppedError{Transport: t.Name(), Sent: sent, Waiting: waiting, Err: err}
		}
		if err != nil {
			report.Refused = append(report.Refused, "not sent "+entry.Wrap+": "+err.Error())
			continue
		}
		report.Sent++
		sent++

		if !isCarrier {
			entry.SentTo = append(entry.SentTo, t.Name())
			if err := m.put(carryBucket, entry.Wrap, entry); err != nil {
				return err
			}
		}
		if entry.Own && entry.Rumor != "" {
			if err := m.attempted(outboxKey(entry.Rumor, entry.To)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Mail) attempted(key string) error {
	// Read first: each write transaction ends with an fsync, and a courier
	// gets again the wraps of an acknowledgement, which has no outbox entry.
	open := false
	if err := m.view(func(tx kv.Tx) error {
		if b := tx.Bucket(outboxBucket); b != nil {
			if data := b.Get([]byte(key)); data != nil {
				var out Outgoing
				if err := json.Unmarshal(data, &out); err != nil {
					return err
				}
				open = out.Delivered.IsZero()
			}
		}
		return nil
	}); err != nil || !open {
		return err
	}
	return m.update(func(tx kv.Tx) error {
		data := tx.Bucket(outboxBucket).Get([]byte(key))
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
		return put(tx, outboxBucket, key, out)
	})
}

func (m *Mail) expire() error {
	return m.update(func(tx kv.Tx) error {
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
		return m.expireDropped(tx)
	})
}

// Message is one message that this citizen received.
type Message struct {
	ID   string
	From nostr.PubKey
	// Members are the sender, then each member that a p tag names. A
	// direct message has two: the sender and this citizen.
	Members []nostr.PubKey
	At      time.Time
	Text    string
	// ReplyTo is the id of the parent of a reply, or empty.
	ReplyTo string
}

// ReplyTo returns the id of the message that a message replies to, in hex,
// or "" for a plain message. A reply holds exactly one e tag, and its second
// element is 64 lowercase hex digits. Further elements, such as the relay
// hint of other NIP-17 clients, do not count.
func ReplyTo(rumor nostr.Event) string {
	parent, n := "", 0
	for _, tag := range rumor.Tags {
		if len(tag) == 0 || tag[0] != "e" {
			continue
		}
		if n++; n == 1 && len(tag) > 1 {
			parent = tag[1]
		}
	}
	if n != 1 || !isHexID(parent) {
		return ""
	}
	return parent
}

// isHexID says whether text is 64 lowercase hex digits.
func isHexID(text string) bool {
	if len(text) != 64 {
		return false
	}
	for _, c := range text {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Members returns the members of a message: its author, then each key that
// a p tag names, once each.
func Members(rumor nostr.Event) []nostr.PubKey {
	members := []nostr.PubKey{rumor.PubKey}
	for tag := range rumor.Tags.FindAll("p") {
		if pk, err := nostr.PubKeyFromHex(tag[1]); err == nil && !slices.Contains(members, pk) {
			members = append(members, pk)
		}
	}
	return members
}

// names says whether a p tag of a rumor names a key.
func names(rumor nostr.Event, pk nostr.PubKey) bool {
	return rumor.Tags.FindWithValue("p", pk.Hex()) != nil
}

// Inbox returns the messages that this citizen received, oldest first. It
// opens each seal from the store, and stores no text. It leaves out a
// message that names this citizen in no p tag.
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
		if rumor.Kind != MessageKind || seen[rumor.ID.Hex()] || !names(rumor, m.key.PublicKey()) {
			continue
		}
		seen[rumor.ID.Hex()] = true
		out = append(out, Message{
			ID: rumor.ID.Hex(), From: rumor.PubKey, Members: Members(rumor),
			At: time.Unix(int64(rumor.CreatedAt), 0).UTC(), Text: rumor.Content,
			ReplyTo: ReplyTo(rumor),
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
		if rumor.Kind == MessageKind && !names(rumor, m.key.PublicKey()) {
			continue
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
	err := m.view(func(tx kv.Tx) error {
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
		out[i].ReplyTo = ReplyTo(rumor)
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

// TooLargeReply is the error that a caller gets in place of a deferred reply
// that is larger than the mail limit.
const TooLargeReply = "the reply is larger than the mail limit; ask for it over a direct path, such as the transfer app"

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
	return m.view(func(tx kv.Tx) error {
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
func put(tx kv.Tx, bucket []byte, key string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), body)
}
func (m *Mail) put(bucket []byte, key string, value any) error {
	return m.update(func(tx kv.Tx) error { return put(tx, bucket, key, value) })
}

func (m *Mail) get(bucket []byte, key string, into any) (bool, error) {
	var body []byte
	err := m.view(func(tx kv.Tx) error {
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
