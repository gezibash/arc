package citizen

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
)

// The contact list is self data, as docs/strangers/SPEC.md, section
// 3.1, defines. Each contact is one draft, so two devices that add a key
// at the same time keep both: the store keeps one version of each d tag,
// the newest (core/store/store.go, settle). The d tag is a keyed value of
// the key, so a draft that reaches a relay does not name the contact.
const (
	contactsApp    = "arc-contacts"
	contactDPrefix = "arc-ct-"
	contactKind    = 30078
)

// ContactDraft says whether an event is a draft of the contact list. Such a
// draft syncs through storage citizens only. A directory that a citizen
// carries can reach a courier, and a draft names its citizen in a p tag.
func ContactDraft(event nostr.Event) bool {
	return event.Kind == draft.Kind && strings.HasPrefix(event.Tags.GetD(), contactDPrefix)
}

// contactRecord is the event inside one contact draft. Contact false
// removes the key.
type contactRecord struct {
	Key     string `json:"key"`
	Contact bool   `json:"contact"`
}

// ContactList is the contact list of a citizen on this machine. It
// implements mail.ContactList.
type ContactList struct{ sess *Session }

// Contacts returns the contact list of this citizen.
func (sess *Session) Contacts() ContactList { return ContactList{sess: sess} }

// Contacts returns each device key on the device list, and each key on the
// contact list. The mail adds the key of the citizen itself.
func (c ContactList) Contacts(ctx context.Context) ([]nostr.PubKey, error) {
	listed, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range c.sess.signers.list().Devices {
		listed = append(listed, d.Key)
	}
	return listed, nil
}

// List returns the keys on the contact list, without the devices.
func (c ContactList) List(ctx context.Context) ([]nostr.PubKey, error) {
	records, err := c.records(ctx)
	if err != nil {
		return nil, err
	}
	var out []nostr.PubKey
	for pk, r := range records {
		if r.on {
			out = append(out, pk)
		}
	}
	return out, nil
}

// AddContacts puts keys on the list. A key that is on it already gets no
// new draft.
func (c ContactList) AddContacts(ctx context.Context, pks []nostr.PubKey) error {
	return c.set(ctx, pks, true)
}

// Remove takes keys off the list.
func (c ContactList) Remove(ctx context.Context, pks []nostr.PubKey) error {
	return c.set(ctx, pks, false)
}

type contactState struct {
	on bool
	at nostr.Timestamp
}

// records reads each contact draft, and keeps the newest record of each
// key. A key can have drafts under two d tags, from two generations of the
// data root.
func (c ContactList) records(ctx context.Context) (map[nostr.PubKey]contactState, error) {
	wraps, err := c.sess.Node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"k": {strconv.Itoa(contactKind)}}})
	if err != nil {
		return nil, err
	}
	opener := c.opener()
	out := map[nostr.PubKey]contactState{}
	for _, wrap := range wraps {
		if !strings.HasPrefix(wrap.Tags.GetD(), contactDPrefix) || draft.Owner(wrap) != c.sess.Key.Public {
			continue
		}
		opened, err := opener.Open(ctx, wrap)
		if err != nil {
			return nil, fmt.Errorf("a contact does not open: %w", err)
		}
		if opened.Deleted {
			continue
		}
		var r contactRecord
		if err := json.Unmarshal([]byte(opened.Event.Content), &r); err != nil {
			return nil, fmt.Errorf("a contact is not valid: %w", err)
		}
		pk, err := nostr.PubKeyFromHex(r.Key)
		if err != nil {
			return nil, fmt.Errorf("a contact is not valid: %w", err)
		}
		if old, ok := out[pk]; !ok || opened.Event.CreatedAt > old.at {
			out[pk] = contactState{on: r.Contact, at: opened.Event.CreatedAt}
		}
	}
	return out, nil
}

func (c ContactList) set(ctx context.Context, pks []nostr.PubKey, on bool) error {
	records, err := c.records(ctx)
	if err != nil {
		return err
	}
	var todo []nostr.PubKey
	for _, pk := range pks {
		if r, ok := records[pk]; on != (ok && r.on) {
			todo = append(todo, pk)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	sealer, err := c.sealer(ctx)
	if err != nil {
		return err
	}
	now := nostr.Now()
	for _, pk := range todo {
		mac := hmac.New(sha256.New, sealer.Key[:])
		mac.Write([]byte("arc-contact-v1\x00" + pk.Hex()))
		d := contactDPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
		body, err := json.Marshal(contactRecord{Key: pk.Hex(), Contact: on})
		if err != nil {
			return err
		}
		// A record must be newer than the one it replaces, also within one
		// second.
		at := now
		if r, ok := records[pk]; ok && r.at >= at {
			at = r.at + 1
		}
		inner := nostr.Event{Kind: contactKind, CreatedAt: at, Tags: nostr.Tags{{"d", d}}, Content: string(body)}
		wrap, err := sealer.Wrap(ctx, d, inner, at)
		if err != nil {
			return err
		}
		if _, err := c.sess.Node.Store.Save(wrap); err != nil {
			return err
		}
	}
	return nil
}

// sealer writes contact drafts under the newest data root of this machine,
// as the self data of an app.
func (c ContactList) sealer(ctx context.Context) (draft.Sealer, error) {
	root, err := c.sess.SealRoot(ctx)
	if err != nil {
		return draft.Sealer{}, err
	}
	key, err := devices.ContentKey(root.Key, contactsApp)
	if err != nil {
		return draft.Sealer{}, err
	}
	return draft.Sealer{Citizen: c.sess.Key.Public, Signer: c.sess.PageSigner(), Gen: root.Gen, Key: key}, nil
}

func (c ContactList) opener() draft.Opener {
	return draft.Opener{
		Citizen: c.sess.Key.Public,
		Keyer:   c.sess.Signer,
		Key: func(ctx context.Context, gen uint32) ([32]byte, error) {
			root, err := c.sess.DataRootAt(ctx, gen)
			if err != nil {
				return [32]byte{}, err
			}
			return devices.ContentKey(root, contactsApp)
		},
	}
}
