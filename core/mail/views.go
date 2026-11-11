package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/private"
)

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
