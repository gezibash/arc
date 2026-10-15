package mail

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
)

// The filter for mail from strangers, as docs/strangers/SPEC.md
// defines. A contact's mail reaches the inbox. A stranger's mail reaches it
// only with proof of work; without, the device holds it apart, and sends no
// acknowledgement.

// MaxHeld caps the held mail of one citizen on one machine. Past it, the
// oldest held seal goes.
const MaxHeld = 200

var (
	heldBucket = []byte("held")
	// droppedBucket names each held seal that the cap pushed out, until its
	// wrap expires. A relay serves the wrap again on each sync, and a
	// dropped seal must not come back and push out another.
	droppedBucket = []byte("dropped")
	// wroteBucket names each key that wrote this citizen a message, or
	// that the scan of stranger looked for.
	wroteBucket = []byte("wrote")
)

// The values of wroteBucket. seededKey is not a key: it marks that the
// contact list holds the recipients of the outbox.
const (
	wroteYes     = "wrote"
	wroteScanned = "scanned"
	seededKey    = "seeded"
)

// ContactList is the contact list of a citizen. Runtime code keeps it as
// self data; core reads no file.
type ContactList interface {
	// Contacts returns each key from which the citizen takes mail without
	// proof of work: its own devices, and each key on its list.
	Contacts(ctx context.Context) ([]nostr.PubKey, error)
	// AddContacts puts keys on the list.
	AddContacts(ctx context.Context, pks []nostr.PubKey) error
}

// held is one seal from a stranger, kept apart from the store until the
// citizen accepts its author, or a wrap of it with work arrives.
type held struct {
	Seal     string    `json:"seal"`
	Author   string    `json:"author"`
	Rumor    string    `json:"rumor"`
	Received time.Time `json:"received"`
	Expires  time.Time `json:"expires"`
}

// filtered says whether the filter applies to a rumor of this kind: a
// message, or the private kind of an app. A call request goes to the gate
// of its app. A reply or an acknowledgement counts only when it matches an
// outbox entry.
func filtered(kind nostr.Kind) bool {
	switch kind {
	case AckKind, call.RequestKind, call.ReplyKind:
		return false
	}
	return true
}

// seed puts each recipient of the outbox on the contact list, once. Before
// the filter, a send added no contact, so without the seed each
// correspondent would be a stranger after an update. A key that the citizen
// removes later stays off.
func (m *Mail) seed(ctx context.Context) error {
	if m.Contacts == nil {
		return nil
	}
	var mark string
	if _, err := m.get(wroteBucket, seededKey, &mark); err != nil || mark != "" {
		return err
	}
	seen := map[nostr.PubKey]bool{}
	var pks []nostr.PubKey
	if err := m.view(func(tx kv.Tx) error {
		return tx.Bucket(outboxBucket).ForEach(func(_, v []byte) error {
			var out Outgoing
			if err := json.Unmarshal(v, &out); err != nil {
				return err
			}
			pk, err := nostr.PubKeyFromHex(out.To)
			if err != nil || out.Kind != "message" || seen[pk] {
				return err
			}
			seen[pk] = true
			pks = append(pks, pk)
			return nil
		})
	}); err != nil {
		return err
	}
	if len(pks) > 0 {
		if err := m.Contacts.AddContacts(ctx, pks); err != nil {
			return err
		}
	}
	return m.put(wroteBucket, seededKey, seededKey)
}

// contacts returns the contacts of this citizen as a set, with its own key.
func (m *Mail) contacts(ctx context.Context) (map[nostr.PubKey]bool, error) {
	set := map[nostr.PubKey]bool{m.key.PublicKey(): true}
	pks, err := m.Contacts.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	for _, pk := range pks {
		set[pk] = true
	}
	return set, nil
}

// admits says whether a wrap of a filtered kind goes to the inbox: its
// author is a contact, or the ID of the wrap has enough work.
func (m *Mail) admits(ctx context.Context, author nostr.PubKey, wrap nostr.Event) (bool, error) {
	if m.Contacts == nil || private.WorkOf(wrap) >= m.work {
		return true, nil
	}
	set, err := m.contacts(ctx)
	return set[author], err
}

// hold keeps the seal of a stranger apart from the store. A seal that is
// held already stays as it is. Past MaxHeld, the oldest seal goes.
func (m *Mail) hold(opened private.Opened, wrap nostr.Event, report *Report) error {
	id := opened.Seal.ID.Hex()
	entry := held{
		Seal: opened.Seal.String(), Author: opened.Author().Hex(), Rumor: opened.Rumor.ID.Hex(),
		Received: m.now().UTC(), Expires: expiresOf(wrap, m.now()).UTC(),
	}
	return m.update(func(tx kv.Tx) error {
		bucket := tx.Bucket(heldBucket)
		if bucket.Get([]byte(id)) != nil || tx.Bucket(droppedBucket).Get([]byte(id)) != nil {
			return nil
		}
		if err := put(tx, heldBucket, id, entry); err != nil {
			return err
		}
		report.Held++
		var all []held
		var ids []string
		if err := bucket.ForEach(func(k, v []byte) error {
			var h held
			if err := json.Unmarshal(v, &h); err != nil {
				return err
			}
			all, ids = append(all, h), append(ids, string(k))
			return nil
		}); err != nil || len(all) <= m.maxHeld {
			return err
		}
		order := make([]int, len(all))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(a, b int) bool { return all[order[a]].Received.Before(all[order[b]].Received) })
		for _, i := range order[:len(all)-m.maxHeld] {
			if err := bucket.Delete([]byte(ids[i])); err != nil {
				return err
			}
			if err := put(tx, droppedBucket, ids[i], all[i].Expires); err != nil {
				return err
			}
		}
		return m.capDropped(tx)
	})
}

// capDropped keeps at most 10 × maxHeld dropped seals (estimated), and
// forgets the ones that expire first. A forgotten seal can be held again.
func (m *Mail) capDropped(tx kv.Tx) error {
	type gone struct {
		id      string
		expires time.Time
	}
	var all []gone
	if err := tx.Bucket(droppedBucket).ForEach(func(k, v []byte) error {
		var at time.Time
		if err := json.Unmarshal(v, &at); err != nil {
			return err
		}
		all = append(all, gone{string(k), at})
		return nil
	}); err != nil || len(all) <= 10*m.maxHeld {
		return err
	}
	sort.Slice(all, func(a, b int) bool { return all[a].expires.Before(all[b].expires) })
	for _, g := range all[:len(all)-10*m.maxHeld] {
		if err := tx.Bucket(droppedBucket).Delete([]byte(g.id)); err != nil {
			return err
		}
	}
	return nil
}

// expireDropped forgets each dropped seal whose wrap expired.
func (m *Mail) expireDropped(tx kv.Tx) error {
	cursor := tx.Bucket(droppedBucket).Cursor()
	for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
		var at time.Time
		if err := json.Unmarshal(v, &at); err != nil {
			return err
		}
		if m.now().After(at) {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
	}
	return nil
}

// unhold removes a held seal, when a wrap of it with work arrives.
func (m *Mail) unhold(seal nostr.ID) error {
	return m.update(func(tx kv.Tx) error { return tx.Bucket(heldBucket).Delete([]byte(seal.Hex())) })
}

// expiresOf is the expiration of a wrap. A wrap without one lives MaxAge.
func expiresOf(wrap nostr.Event, now time.Time) time.Time {
	if tag := wrap.Tags.Find("expiration"); tag != nil {
		if at, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
			return time.Unix(at, 0)
		}
	}
	return now.Add(private.MaxAge)
}

// Release moves the held mail of each contact to the store, and
// acknowledges it. arc contacts add calls it, and each sync does, so a
// contact added on another device releases the mail here after the list
// syncs.
func (m *Mail) Release(ctx context.Context) (Report, error) {
	report := Report{}
	m.receiving.Lock()
	defer m.receiving.Unlock()
	if err := m.seed(ctx); err != nil {
		return report, err
	}
	return report, m.release(ctx, &report)
}

// release moves the held mail of each contact to the store, and drops the
// held mail whose wrap expired. The caller holds the receiving lock.
func (m *Mail) release(ctx context.Context, report *Report) error {
	entries, err := m.heldEntries()
	if err != nil || len(entries) == 0 {
		return err
	}
	set := map[nostr.PubKey]bool{}
	if m.Contacts != nil {
		if set, err = m.contacts(ctx); err != nil {
			return err
		}
	}
	for id, h := range entries {
		author, err := nostr.PubKeyFromHex(h.Author)
		if err != nil {
			return err
		}
		switch {
		case m.now().After(h.Expires):
			if err := m.update(func(tx kv.Tx) error { return tx.Bucket(heldBucket).Delete([]byte(id)) }); err != nil {
				return err
			}
		case m.Contacts == nil || set[author]:
			if err := m.accept(ctx, id, h, author, report); err != nil {
				return err
			}
		}
	}
	return nil
}

// accept moves one held seal to the store, and acknowledges its rumor.
func (m *Mail) accept(ctx context.Context, id string, h held, author nostr.PubKey, report *Report) error {
	seal, err := private.OpenSealJSON(h.Seal)
	if err != nil {
		return err
	}
	result, err := m.node.Store.Save(seal)
	if err != nil {
		return err
	}
	if result.Outcome != store.Refused {
		report.Received++
		ack := private.Rumor(m.key, AckKind, "", nostr.Tags{{"e", h.Rumor}}, m.now())
		if _, err := m.post(ctx, author, ack, m.now().Add(private.MaxAge)); err != nil {
			return err
		}
	}
	return m.update(func(tx kv.Tx) error { return tx.Bucket(heldBucket).Delete([]byte(id)) })
}

func (m *Mail) heldEntries() (map[string]held, error) {
	entries := map[string]held{}
	err := m.view(func(tx kv.Tx) error {
		return tx.Bucket(heldBucket).ForEach(func(k, v []byte) error {
			var h held
			if err := json.Unmarshal(v, &h); err != nil {
				return err
			}
			entries[string(k)] = h
			return nil
		})
	})
	return entries, err
}

// Holding counts the held mail that has not expired. It opens no seal.
func (m *Mail) Holding() (int, error) {
	entries, err := m.heldEntries()
	count := 0
	for _, h := range entries {
		if !m.now().After(h.Expires) {
			count++
		}
	}
	return count, err
}

// Held returns the held mail that has not expired, oldest first. It opens
// each seal, so on a device each one asks the signer.
func (m *Mail) Held(ctx context.Context) ([]Message, error) {
	entries, err := m.heldEntries()
	if err != nil {
		return nil, err
	}
	var out []Message
	for _, h := range entries {
		if m.now().After(h.Expires) {
			continue
		}
		seal, err := private.OpenSealJSON(h.Seal)
		if err != nil {
			return nil, err
		}
		rumor, err := private.OpenSeal(ctx, m.key, seal)
		if err != nil {
			return nil, err
		}
		out = append(out, Message{
			ID: rumor.ID.Hex(), From: rumor.PubKey, Members: Members(rumor),
			At: time.Unix(int64(rumor.CreatedAt), 0).UTC(), Text: rumor.Content,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out, nil
}

// wrote records that a key wrote this citizen a message. That key put this
// citizen on its contact list when it sealed the message.
func (m *Mail) wrote(author nostr.PubKey) error {
	var mark string
	if _, err := m.get(wroteBucket, author.Hex(), &mark); err != nil || mark == wroteYes {
		return err
	}
	return m.put(wroteBucket, author.Hex(), wroteYes)
}

// stranger says whether this citizen is a stranger to a recipient: the
// recipient never wrote it a message. Mail from before the filter has no
// record, so the first send to a key scans the seals that the key wrote,
// once.
func (m *Mail) stranger(ctx context.Context, to nostr.PubKey) (bool, error) {
	var mark string
	if _, err := m.get(wroteBucket, to.Hex(), &mark); err != nil {
		return false, err
	}
	switch mark {
	case wroteYes:
		return false, nil
	case wroteScanned:
		return true, nil
	}
	seals, err := m.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.SealKind}, Authors: []nostr.PubKey{to}})
	if err != nil {
		return false, err
	}
	mark = wroteScanned
	for _, seal := range seals {
		rumor, err := private.OpenSeal(ctx, m.key, seal)
		if err != nil {
			return false, err
		}
		if filtered(rumor.Kind) && (rumor.Kind != MessageKind || names(rumor, m.key.PublicKey())) {
			mark = wroteYes
			break
		}
	}
	return mark == wroteScanned, m.put(wroteBucket, to.Hex(), mark)
}
