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
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/private"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
	"go.etcd.io/bbolt"
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
)

// Outgoing is one message in the outbox.
type Outgoing struct {
	Rumor     string    `json:"rumor"`
	To        string    `json:"to"`
	Seal      string    `json:"seal"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	Attempts  int       `json:"attempts"`
	Delivered time.Time `json:"delivered,omitzero"`

	// Text is the message, opened from the sender's own seal. It is never
	// stored.
	Text string `json:"-"`
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
	key    keys.Key
	node   *node.Node
	db     *bbolt.DB
	relays []transport.Transport
	now    func() time.Time
}

// Open opens the mail state in a directory. The relays are where new mail is
// sent at once.
func Open(dir string, k keys.Key, n *node.Node, relays []transport.Transport) (*Mail, error) {
	db, err := bbolt.Open(filepath.Join(dir, "mail.db"), 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("mail: %w", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{outboxBucket, carryBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("mail: %w", err)
	}
	return &Mail{key: k, node: n, db: db, relays: relays, now: time.Now}, nil
}

// Close releases the mail state.
func (m *Mail) Close() error { return m.db.Close() }

// Send seals a message to a citizen, puts it in the outbox, and sends it to
// the relays at once. A relay that fails does not fail the send: the outbox
// sends the message again on the next sync.
func (m *Mail) Send(ctx context.Context, to nostr.PubKey, text string) (Outgoing, error) {
	if to == m.key.Public {
		return Outgoing{}, errors.New("mail: a message to yourself belongs in the journal")
	}

	now := m.now()
	rumor := private.Rumor(m.key, MessageKind, text, nostr.Tags{{"p", to.Hex()}}, now)
	expires := now.Add(private.MaxAge)

	seal, err := m.post(ctx, to, rumor, expires, true)
	if err != nil {
		return Outgoing{}, err
	}

	out := Outgoing{
		Rumor: rumor.ID.Hex(), To: to.Hex(), Seal: seal.ID.Hex(),
		Created: now.UTC(), Expires: expires.UTC(), Text: text,
	}
	return out, m.put(outboxBucket, out.Rumor, out)
}

// post seals a rumor, keeps the seal, and makes both wraps this node's own
// mail. It sends both wraps to the relays.
func (m *Mail) post(ctx context.Context, to nostr.PubKey, rumor nostr.Event, expires time.Time, keepSeal bool) (nostr.Event, error) {
	seal, err := private.Seal(m.key, to, rumor)
	if err != nil {
		return nostr.Event{}, err
	}
	if keepSeal {
		if _, err := m.node.Store.Save(seal); err != nil {
			return nostr.Event{}, err
		}
	}

	for _, form := range []private.Form{private.CourierForm, private.RelayForm} {
		wrap, err := private.WrapSeal(seal, to, form, private.WrapKind, expires)
		if err != nil {
			return nostr.Event{}, err
		}
		if result, err := m.node.Store.Save(wrap); err != nil || result.Outcome == store.Refused {
			return nostr.Event{}, fmt.Errorf("mail: the store refused a wrap: %v %s", err, result.Reason)
		}

		entry := carried{
			Wrap: wrap.ID.Hex(), Own: true, Courier: form == private.CourierForm,
			Hops: StartHops, Received: m.now().UTC(), Expires: expires.UTC(), Rumor: rumor.ID.Hex(),
		}
		for _, r := range m.relays {
			if r.Send(ctx, wrap) == nil {
				entry.SentTo = append(entry.SentTo, r.Name())
			}
		}
		if err := m.put(carryBucket, entry.Wrap, entry); err != nil {
			return nostr.Event{}, err
		}
	}
	return seal, nil
}

// Report says what one sync of mail did.
type Report struct {
	Transport string
	// Received counts messages for this citizen that arrived.
	Received int
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
	m.expire()

	carrier, isCarrier := t.(transport.Carrier)
	mine := private.RouteTags(m.key.Public, m.now())

	var filters []nostr.Filter
	if isCarrier {
		filters = []nostr.Filter{{Kinds: []nostr.Kind{private.WrapKind}}}
	} else {
		filters = []nostr.Filter{
			{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {m.key.Public.Hex()}}},
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

	if forMe(wrap, m.key.Public, mine) {
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
// acknowledged. A seal that the store already held was handled before.
func (m *Mail) open(ctx context.Context, wrap nostr.Event, report *Report) error {
	opened, err := private.Unwrap(m.key, wrap)
	if err != nil {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": "+err.Error())
		return nil
	}

	result, err := m.node.Store.Save(opened.Seal)
	if err != nil {
		return err
	}
	if result.Outcome == store.Duplicate {
		return nil
	}
	if result.Outcome == store.Refused {
		report.Refused = append(report.Refused, wrap.ID.Hex()+": "+result.Reason)
		return nil
	}

	switch opened.Rumor.Kind {
	case AckKind:
		return m.acknowledged(opened, report)
	case MessageKind:
		report.Received++
		ack := private.Rumor(m.key, AckKind, "", nostr.Tags{{"e", opened.Rumor.ID.Hex()}}, m.now())
		_, err := m.post(ctx, opened.Author(), ack, m.now().Add(private.MaxAge), false)
		return err
	}
	return nil
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
	m.each(func(c carried) {
		if !c.Own {
			others = append(others, c)
		}
	})
	if len(others) <= MaxCarried {
		return nil
	}

	sort.Slice(others, func(a, b int) bool { return others[a].Received.Before(others[b].Received) })
	return m.db.Update(func(tx *bbolt.Tx) error {
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
	m.each(func(c carried) { entries = append(entries, c) })

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
		events := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{id}})
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
			m.attempted(entry.Rumor)
		}
	}
	return nil
}

func (m *Mail) attempted(rumor string) {
	var out Outgoing
	if found, err := m.get(outboxBucket, rumor, &out); err == nil && found && out.Delivered.IsZero() {
		out.Attempts++
		_ = m.put(outboxBucket, rumor, out)
	}
}

// expire drops each carried wrap whose life has passed. The outbox keeps
// expired messages, so the citizen can see that they were not delivered.
func (m *Mail) expire() {
	now := m.now()
	_ = m.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(carryBucket)
		var gone [][]byte
		_ = bucket.ForEach(func(k, v []byte) error {
			var c carried
			if json.Unmarshal(v, &c) == nil && now.After(c.Expires) {
				gone = append(gone, append([]byte(nil), k...))
			}
			return nil
		})
		for _, k := range gone {
			_ = bucket.Delete(k)
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
func (m *Mail) Inbox() []Message {
	var out []Message
	seen := map[string]bool{}

	for _, seal := range m.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.SealKind}}) {
		if seal.PubKey == m.key.Public {
			continue
		}
		rumor, err := private.OpenSeal(m.key, seal)
		if err != nil || rumor.Kind != MessageKind || seen[rumor.ID.Hex()] {
			continue
		}
		seen[rumor.ID.Hex()] = true
		out = append(out, Message{
			ID: rumor.ID.Hex(), From: rumor.PubKey,
			At: time.Unix(int64(rumor.CreatedAt), 0).UTC(), Text: rumor.Content,
		})
	}

	sort.Slice(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out
}

// Outbox returns the messages that this citizen sent, oldest first.
func (m *Mail) Outbox() []Outgoing {
	var out []Outgoing
	_ = m.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(outboxBucket).ForEach(func(_, v []byte) error {
			var o Outgoing
			if json.Unmarshal(v, &o) == nil {
				o.Text = m.ownText(o)
				out = append(out, o)
			}
			return nil
		})
	})
	sort.Slice(out, func(a, b int) bool { return out[a].Created.Before(out[b].Created) })
	return out
}

func (m *Mail) ownText(o Outgoing) string {
	id, err := nostr.IDFromHex(o.Seal)
	if err != nil {
		return ""
	}
	to, err := nostr.PubKeyFromHex(o.To)
	if err != nil {
		return ""
	}
	seals := m.node.Store.Query(nostr.Filter{IDs: []nostr.ID{id}})
	if len(seals) == 0 {
		return ""
	}
	rumor, err := private.OpenOwnSeal(m.key, seals[0], to)
	if err != nil {
		return ""
	}
	return rumor.Content
}

// Carrying counts the wraps of other citizens that this node carries.
func (m *Mail) Carrying() int {
	count := 0
	m.each(func(c carried) {
		if !c.Own {
			count++
		}
	})
	return count
}

func (m *Mail) each(fn func(carried)) {
	_ = m.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(carryBucket).ForEach(func(_, v []byte) error {
			var c carried
			if json.Unmarshal(v, &c) == nil {
				fn(c)
			}
			return nil
		})
	})
}

func (m *Mail) put(bucket []byte, key string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return m.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), body)
	})
}

func (m *Mail) get(bucket []byte, key string, into any) (bool, error) {
	var body []byte
	err := m.db.View(func(tx *bbolt.Tx) error {
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
