package store_test

import (
	"context"
	"slices"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/internal/testutil"
)

// list is a device list for the store: the listed keys, and one revoked
// key with the time and the first generation that it never held.
type list struct {
	citizen nostr.PubKey
	listed  []nostr.PubKey
	revoked nostr.PubKey
	at      nostr.Timestamp
	gen     uint32
	none    bool
}

func (l list) Standing(citizen, signer nostr.PubKey) store.Standing {
	switch {
	case citizen != l.citizen:
		return store.Standing{}
	case l.none:
		return store.Standing{NoList: true}
	case signer == l.revoked:
		return store.Standing{RevokedAt: l.at, RevokedGen: l.gen}
	}
	return store.Standing{Listed: slices.Contains(l.listed, signer)}
}

func page(t *testing.T, citizen nostr.PubKey, device keys.Key, gen uint32, d, text string, at nostr.Timestamp) nostr.Event {
	t.Helper()
	s := draft.Sealer{Citizen: citizen, Signer: device, Gen: gen, Key: [32]byte{1}}
	wrap, err := s.Wrap(context.Background(), d, nostr.Event{Kind: 30023, CreatedAt: at, Content: text}, at)
	if err != nil {
		t.Fatal(err)
	}
	return wrap
}

// Two devices that write one page make one page in the store, and a
// deletion that one device signs removes the page of the other.
func TestSelfDataBelongsToTheCitizen(t *testing.T) {
	citizen, phone, laptop := keys.Generate(), keys.Generate(), keys.Generate()
	s := open(t)
	s.SetSigners(list{citizen: citizen.Public, listed: []nostr.PubKey{phone.Public, laptop.Public}})

	older := page(t, citizen.Public, phone, 1, "p1", "from the phone", 1000)
	newer := page(t, citizen.Public, laptop, 1, "p1", "from the laptop", 1001)
	if r := save(t, s, older); r.Outcome != store.Stored {
		t.Fatalf("the page of the phone: %v %s", r.Outcome, r.Reason)
	}
	if r := save(t, s, newer); r.Outcome != store.Stored {
		t.Fatalf("the page of the laptop: %v %s", r.Outcome, r.Reason)
	}
	wraps := testutil.Must(s.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"p": {citizen.Public.Hex()}}}))
	if len(wraps) != 1 || wraps[0].ID != newer.ID {
		t.Fatalf("the store holds %d versions of one page, want only the newer one", len(wraps))
	}
	if r := save(t, s, older); r.Outcome != store.Superseded {
		t.Fatalf("the older page of the other device came back: %v", r.Outcome)
	}

	s2 := draft.Sealer{Citizen: citizen.Public, Signer: phone, Gen: 1}
	request, err := s2.Deletion(context.Background(), nostr.Tags{{"e", newer.ID.Hex()}, {"k", "31234"}}, 1002)
	if err != nil {
		t.Fatal(err)
	}
	if r := save(t, s, request); r.Outcome != store.Stored {
		t.Fatalf("the deletion of the phone: %v %s", r.Outcome, r.Reason)
	}
	if testutil.Must(s.Has(newer.ID)) {
		t.Fatal("a deletion of the phone did not remove the page of the laptop")
	}
	if r := save(t, s, newer); r.Outcome != store.Refused {
		t.Fatalf("the deleted page came back: %v", r.Outcome)
	}

	// A p tag of another kind is a mention, not an owner.
	note := signed(t, phone, 1, 1000, "hello", nostr.Tag{"p", citizen.Public.Hex()})
	if draft.Owner(note) != phone.Public {
		t.Fatal("a note that mentions the citizen belongs to the citizen")
	}
}

// The store takes self data from a key that is not the citizen only when
// the device list allows it.
func TestSelfDataNeedsADeviceOnTheList(t *testing.T) {
	citizen, phone, thief := keys.Generate(), keys.Generate(), keys.Generate()
	const revokedAt = 2000
	l := list{citizen: citizen.Public, listed: []nostr.PubKey{phone.Public}, revoked: thief.Public, at: revokedAt, gen: 3}

	for _, c := range []struct {
		name  string
		list  store.Signers
		event nostr.Event
		from  store.Source
		want  store.Outcome
	}{
		{"a listed device", l, page(t, citizen.Public, phone, 1, "a", "x", 1500), store.Elsewhere, store.Stored},
		{"the citizen key", nil, page(t, citizen.Public, citizen, 0, "b", "x", 1500), store.Elsewhere, store.Stored},
		{"no device list given", nil, page(t, citizen.Public, phone, 1, "c", "x", 1500), store.Elsewhere, store.Refused},
		{"a key not on the list", l, page(t, citizen.Public, keys.Generate(), 1, "d", "x", 1500), store.StorageCitizen, store.Refused},
		{"a revoked key, before, from a stick", l, page(t, citizen.Public, thief, 2, "e", "x", 1500), store.Elsewhere, store.Refused},
		{"a revoked key, before, from a storage citizen", l, page(t, citizen.Public, thief, 2, "f", "x", 1500), store.StorageCitizen, store.Stored},
		{"a revoked key, at the revocation", l, page(t, citizen.Public, thief, 2, "g", "x", revokedAt), store.StorageCitizen, store.Refused},
		{"a revoked key, after", l, page(t, citizen.Public, thief, 2, "h", "x", 2500), store.StorageCitizen, store.Refused},
		{"a revoked key, a generation it never held", l, page(t, citizen.Public, thief, 3, "i", "x", 1500), store.StorageCitizen, store.Refused},
		{"a citizen with no list", list{citizen: citizen.Public, none: true}, page(t, citizen.Public, phone, 0, "j", "x", 1500), store.Elsewhere, store.Stored},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)
			if c.list != nil {
				s.SetSigners(c.list)
			}
			r, err := s.SaveFrom(c.event, c.from)
			if err != nil {
				t.Fatal(err)
			}
			if r.Outcome != c.want {
				t.Fatalf("outcome %v (%s), want %v", r.Outcome, r.Reason, c.want)
			}
		})
	}
}
