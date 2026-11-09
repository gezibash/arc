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
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
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
