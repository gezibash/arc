package selfsync

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/sdk/storage"
)

// memStorage is a storage app in memory. It keeps tags, as the feature
// pieces says.
type memStorage struct {
	mu      sync.Mutex
	seq     uint64
	entries map[string]storage.Entry
	data    map[string][]byte
	puts    int
}

func newMemStorage() *memStorage {
	return &memStorage{entries: map[string]storage.Entry{}, data: map[string][]byte{}}
}

func (m *memStorage) Pieces(context.Context) (bool, error) { return true, nil }

func (m *memStorage) List(_ context.Context, after uint64) ([]storage.Entry, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []storage.Entry
	for _, e := range m.entries {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b storage.Entry) int { return int(a.Seq) - int(b.Seq) })
	next := after
	if len(out) > 0 {
		next = out[len(out)-1].Seq
	}
	return out, next, nil
}

func (m *memStorage) GetMany(_ context.Context, ids []string, f func(string, []byte, error) error) error {
	for _, id := range ids {
		m.mu.Lock()
		data, ok := m.data[id]
		m.mu.Unlock()
		var err error
		if !ok {
			err = &storage.Refused{Code: storage.ErrNotFound, Message: "not found"}
		}
		if err := f(id, data, err); err != nil {
			return err
		}
	}
	return nil
}

func (m *memStorage) PutMany(_ context.Context, blobs []storage.Blob) ([]storage.PutResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]storage.PutResult, len(blobs))
	for i, b := range blobs {
		if _, ok := m.data[b.ID]; ok {
			out[i].Status = storage.StatusHeld
			continue
		}
		m.seq++
		m.puts++
		m.data[b.ID] = b.Data
		m.entries[b.ID] = storage.Entry{Seq: m.seq, ID: b.ID, Size: int64(len(b.Data)), Tag: encodeTag(b.Tag)}
		out[i].Status = storage.StatusStored
	}
	return out, nil
}

func encodeTag(tag []byte) string {
	if tag == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(tag)
}

// memStore is a local store in memory.
type memStore struct{ events map[nostr.ID]nostr.Event }

func (s *memStore) Save(e nostr.Event) (store.Result, error) {
	if _, ok := s.events[e.ID]; ok {
		return store.Result{Outcome: store.Duplicate}, nil
	}
	s.events[e.ID] = e
	return store.Result{Outcome: store.Stored}, nil
}

func (s *memStore) Query(nostr.Filter) ([]nostr.Event, error) {
	return slices.Collect(maps.Values(s.events)), nil
}

func testSyncer(t *testing.T, citizen keys.Key, local *memStore) *Syncer {
	t.Helper()
	k := testKeys(t, 0)
	return &Syncer{
		Citizen: citizen.Public, Device: citizen.Secret, Store: local, State: t.TempDir(),
		Newest:  func(context.Context) (Keys, error) { return k, nil },
		Keys:    func(context.Context, uint32) (Keys, error) { return k, nil },
		Devices: func(context.Context) (devices.List, bool, error) { return devices.List{}, false, nil },
	}
}

// TestNewStorageCitizenAddsPiece checks section 4.1 of the storage spec. A
// storage citizen that did not answer when a record was coded gets its
// piece at a later sync, and so does a storage citizen that the citizen
// adds later. No other piece changes. A second device then rebuilds each
// record from the two added pieces alone.
func TestNewStorageCitizenAddsPiece(t *testing.T) {
	ctx := context.Background()
	citizen := keys.Generate()
	local := &memStore{events: map[nostr.ID]nostr.Event{}}
	for n := range 3 {
		e := nostr.Event{Kind: draft.Kind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", fmt.Sprintf("page/%d", n)}}, Content: fmt.Sprintf("page %d", n)}
		if err := e.Sign(citizen.Secret); err != nil {
			t.Fatal(err)
		}
		local.events[e.ID] = e
	}
	stores := make([]*memStorage, 4)
	sides := make([]Side, 4)
	for i := range stores {
		stores[i] = newMemStorage()
		sides[i] = Side{Key: keys.Generate().Public, Label: fmt.Sprint("s", i), Conn: stores[i], Active: true}
	}
	device := testSyncer(t, citizen, local)
	syncOn := func(s *Syncer, sides []Side) Report {
		t.Helper()
		r, err := s.SyncAll(ctx, sides)
		if err != nil {
			t.Fatal(err)
		}
		for i, side := range r.Sides {
			if side.Err != nil {
				t.Fatalf("side %d: %v", i, side.Err)
			}
		}
		return r
	}
	puts := func() []int {
		out := make([]int, len(stores))
		for i, s := range stores {
			out[i] = s.puts
		}
		return out
	}

	// Three storage citizens, and the third does not answer: k = 2, and
	// each record gets 2 pieces.
	first := slices.Clone(sides[:3])
	first[2].Conn = nil
	r := syncOn(device, first)
	if r.K != 2 || r.T != 3 || r.Sent != 3 {
		t.Fatalf("the first sync coded %d records as %d of %d; want 3 as 2 of 3", r.Sent, r.K, r.T)
	}
	if got := puts(); !slices.Equal(got, []int{3, 3, 0, 0}) {
		t.Fatalf("the first sync put %v pieces; want 3 on each of the 2 that answer", got)
	}

	// The third answers through TURN: a missing piece is repair, and
	// waits for a direct path.
	relayed := slices.Clone(sides[:3])
	relayed[2].Relayed = true
	syncOn(device, relayed)
	if got := puts(); !slices.Equal(got, []int{3, 3, 0, 0}) {
		t.Fatalf("a sync through TURN put %v pieces; want no missing piece through TURN", got)
	}

	// The third answers on a direct path: it gets the missing piece of
	// each blob. The other pieces stay.
	r = syncOn(device, sides[:3])
	if r.Sent != 0 {
		t.Fatalf("the second sync coded %d records again", r.Sent)
	}
	if got := puts(); !slices.Equal(got, []int{3, 3, 3, 0}) {
		t.Fatalf("after the second sync the storage citizens hold %v pieces; want 3 on each of 3", got)
	}

	// A fourth storage citizen joins: S = 4, and a blob of k = 2 gets a
	// fourth piece.
	r = syncOn(device, sides)
	if r.Sent != 0 || r.K != 2 || r.T != 4 {
		t.Fatalf("the sync with a fourth storage citizen coded %d records, as %d of %d", r.Sent, r.K, r.T)
	}
	if got := puts(); !slices.Equal(got, []int{3, 3, 3, 3}) {
		t.Fatalf("after the fourth joined the storage citizens hold %v pieces; want 3 on each of 4", got)
	}

	// A second device reads from the 2 storage citizens that got their
	// pieces late.
	other := &memStore{events: map[nostr.ID]nostr.Event{}}
	reader := testSyncer(t, citizen, other)
	late := slices.Clone(sides)
	late[0].Conn, late[1].Conn = nil, nil
	r = syncOn(reader, late)
	if r.Received != 3 || len(r.Unreadable) != 0 || len(r.Refused) != 0 {
		t.Fatalf("the second device took %d records from the late pieces; unreadable %v, refused %v", r.Received, r.Unreadable, r.Refused)
	}
}
