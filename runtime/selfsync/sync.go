package selfsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/sdk/atomicfile"
	"github.com/gezibash/arc/sdk/storage"
)

// Kinds are the kinds of self data. They stay in the local store, and go
// only to storage citizens, as blobs.
var Kinds = []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, nostr.KindDeletion}

// Filter matches the self data of a citizen in the local store.
func Filter(me nostr.PubKey) nostr.Filter {
	return nostr.Filter{Kinds: Kinds, Authors: []nostr.PubKey{me}}
}

// errNotListed refuses a change of a device key that is not on the device
// list.
var errNotListed = errors.New("the device key is not on the device list")

// Store is the local store of the device.
type Store interface {
	Save(nostr.Event) (store.Result, error)
	Query(nostr.Filter) ([]nostr.Event, error)
}

// Blobs is one open session with a storage citizen. *storage.Conn is one.
// GetMany and PutMany keep several requests in flight on a storage app
// that supports it, and send one request at a time to an older one.
type Blobs interface {
	List(ctx context.Context, after uint64) ([]storage.Entry, uint64, error)
	GetMany(ctx context.Context, ids []string, f func(id string, blob []byte, err error) error) error
	PutMany(ctx context.Context, blobs []storage.Blob) ([]storage.PutResult, error)
}

// putChunk is the count of records that a device seals before it puts
// them.
const putChunk = 64

// Syncer syncs the self data of one citizen on one device.
type Syncer struct {
	Citizen nostr.PubKey
	// Device is the key that signs each change: the device key, or the
	// citizen key on the machine that holds it.
	Device nostr.SecretKey
	Store  Store
	// Newest returns the keys of the newest generation that this device
	// holds. The device seals each new blob with them.
	Newest func(ctx context.Context) (Keys, error)
	// Keys returns the keys of one generation, to open a blob of it. It
	// fails for a generation that this device does not hold.
	Keys func(ctx context.Context, gen uint32) (Keys, error)
	// Devices returns the device list of the citizen, and false when the
	// citizen has none.
	Devices func(ctx context.Context) (devices.List, bool, error)
	// State is the directory of the cursor of each storage citizen.
	State string
}

// Report says what one sync did.
type Report struct {
	// Received is the number of records that the store took.
	Received int
	// Held is the number of records that the store already held, or held
	// a newer version of.
	Held int
	// Sent is the number of blobs put.
	Sent int
	// Refused holds one reason for each blob that the device did not take.
	Refused []string
}

// state is what a device remembers of one storage citizen.
type state struct {
	// Cursor is the last sequence number that the device listed.
	Cursor uint64 `json:"cursor"`
	// Remote holds each ID that the storage citizen listed: true if it holds
	// the blob, false if the blob was deleted. The device puts no ID that
	// it holds.
	Remote map[string]bool `json:"remote"`
	// Seen holds the IDs that the device got, whatever the store did.
	Seen map[string]bool `json:"seen"`
	// Answered is the Unix time of the last pipe that opened.
	Answered int64 `json:"answered,omitempty"`
	// SilentSince is the Unix time of the first failed open after the
	// last pipe that opened. It is 0 while the storage citizen answers.
	SilentSince int64 `json:"silent_since,omitempty"`
}

// SilentWarning is the time with no answer after which each sync warns
// that a storage citizen is silent. Only the user marks it gone.
const SilentWarning = 7 * 24 * time.Hour

// Answered records that a pipe to the storage citizen opened at the time at.
func (s *Syncer) Answered(name string, at time.Time) error {
	st, err := s.load(name)
	if err != nil {
		return err
	}
	st.Answered, st.SilentSince = at.Unix(), 0
	return s.save(name, st)
}

// Unanswered records that no pipe to the storage citizen opened at the time
// at. It returns the time since the first failed open after the last
// answer: a device that was off itself does not count that time.
func (s *Syncer) Unanswered(name string, at time.Time) (time.Duration, error) {
	st, err := s.load(name)
	if err != nil {
		return 0, err
	}
	if st.SilentSince == 0 {
		st.SilentSince = at.Unix()
		if err := s.save(name, st); err != nil {
			return 0, err
		}
	}
	return at.Sub(time.Unix(st.SilentSince, 0)), nil
}

func (s *Syncer) statePath(name string) string {
	return filepath.Join(s.State, name+".json")
}

func (s *Syncer) load(name string) (state, error) {
	st := state{Remote: map[string]bool{}, Seen: map[string]bool{}}
	raw, err := os.ReadFile(s.statePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("%s: %w", s.statePath(name), err)
	}
	if st.Remote == nil {
		st.Remote = map[string]bool{}
	}
	if st.Seen == nil {
		st.Seen = map[string]bool{}
	}
	return st, nil
}

func (s *Syncer) save(name string, st state) error {
	if err := os.MkdirAll(s.State, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.statePath(name), raw, 0o600)
}

// Sync syncs with one storage citizen. name names the storage citizen in
// the state directory, such as its key in hex. The device first gets the
// blobs that it lacks, and then, if put is true, puts the records that the
// storage citizen lacks. A deletion that it gets first thus drops the old
// records from the store before the device would put them back. A storage
// citizen that the citizen drains gets no put.
func (s *Syncer) Sync(ctx context.Context, name string, b Blobs, put bool) (Report, error) {
	var report Report
	st, err := s.load(name)
	if err != nil {
		return report, err
	}
	current, err := s.Newest(ctx)
	if err != nil {
		return report, err
	}
	// Each generation once for each sync: the keys of a generation can
	// need the relays and the signer.
	held := map[uint32]Keys{current.Gen: current}
	keysOf := func(ctx context.Context, gen uint32) (Keys, error) {
		if k, ok := held[gen]; ok {
			return k, nil
		}
		k, err := s.Keys(ctx, gen)
		if err == nil {
			held[gen] = k
		}
		return k, err
	}

	// The local records, by blob ID. The ID key is the same in each
	// generation.
	records, err := s.Store.Query(Filter(s.Citizen))
	if err != nil {
		return report, err
	}
	local := make(map[string]bool, len(records))
	for _, event := range records {
		local[current.BlobID(event.ID)] = true
	}

	entries, next, err := b.List(ctx, st.Cursor)
	for _, e := range entries {
		st.Remote[e.ID] = !e.Deleted
	}
	st.Cursor = next
	if err != nil {
		return report, errors.Join(err, s.save(name, st))
	}

	// The device reads the device list only if it takes a blob. An absent
	// device list is an empty slice.
	var list []nostr.PubKey
	// Each blob that the device lacks, also one of an earlier listing that
	// a device that was not on the list made.
	var lack []string
	for id, present := range st.Remote {
		if present && !local[id] && !st.Seen[id] {
			lack = append(lack, id)
		}
	}
	slices.Sort(lack)
	if len(lack) > 0 {
		l, ok, err := s.Devices(ctx)
		if err != nil {
			return report, errors.Join(fmt.Errorf("the device list: %w", err), s.save(name, st))
		}
		list = []nostr.PubKey{}
		if ok {
			// The citizen key may always write.
			list = append(list, s.Citizen)
			for _, d := range l.Devices {
				list = append(list, d.Key)
			}
		}
	}
	err = b.GetMany(ctx, lack, func(id string, blob []byte, err error) error {
		var result store.Result
		if err == nil {
			result, err = s.take(ctx, id, blob, list, keysOf)
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			report.Refused = append(report.Refused, fmt.Sprintf("%s: %v", id, err))
			// A device can join the list later, so its blob gets a new
			// chance on the next sync.
			if !errors.Is(err, errNotListed) {
				st.Seen[id] = true
			}
			return nil
		}
		st.Seen[id] = true
		switch result.Outcome {
		case store.Stored:
			report.Received++
		case store.Refused:
			report.Refused = append(report.Refused, fmt.Sprintf("%s: the store refused it: %s", id, result.Reason))
		default:
			report.Held++
		}
		return nil
	})
	if err != nil {
		return report, errors.Join(err, s.save(name, st))
	}

	if !put {
		return report, s.save(name, st)
	}

	// Read the store again: a deletion that came in removed records.
	records, err = s.Store.Query(Filter(s.Citizen))
	if err != nil {
		return report, errors.Join(err, s.save(name, st))
	}
	var unlisted []nostr.Event
	for _, event := range records {
		if _, listed := st.Remote[current.BlobID(event.ID)]; !listed {
			unlisted = append(unlisted, event)
		}
	}
	// The puts go in chunks, so that a device seals at most putChunk
	// records before it gives them.
	for chunk := range slices.Chunk(unlisted, putChunk) {
		blobs := make([]storage.Blob, len(chunk))
		for i, event := range chunk {
			id, blob, err := Seal(current, s.Device, event)
			if err != nil {
				return report, errors.Join(err, s.save(name, st))
			}
			blobs[i] = storage.Blob{ID: id, Data: blob}
		}
		results, err := b.PutMany(ctx, blobs)
		var failed []error
		for i, r := range results {
			switch {
			case r.Err != nil:
				failed = append(failed, fmt.Errorf("put %s: %w", blobs[i].ID, r.Err))
			case r.Status != "":
				st.Remote[blobs[i].ID] = r.Status != storage.StatusTombstoned
				if r.Status == storage.StatusStored {
					report.Sent++
				}
			}
		}
		if err != nil || len(failed) > 0 {
			return report, errors.Join(err, errors.Join(failed...), s.save(name, st))
		}
	}
	return report, s.save(name, st)
}

// take opens one blob, checks who made it, and gives the record to the
// store.
func (s *Syncer) take(ctx context.Context, id string, blob []byte, list []nostr.PubKey, keysOf func(context.Context, uint32) (Keys, error)) (store.Result, error) {
	gen, err := Gen(blob)
	if err != nil {
		return store.Result{}, err
	}
	keys, err := keysOf(ctx, gen)
	if err != nil {
		return store.Result{}, err
	}
	env, err := Open(keys, id, blob)
	if err != nil {
		return store.Result{}, err
	}
	if env.Record.PubKey != s.Citizen {
		return store.Result{}, errors.New("the record is of another citizen")
	}
	if !isSelf(env.Record.Kind) {
		return store.Result{}, fmt.Errorf("the record has kind %d, which is not self data", env.Record.Kind)
	}
	// With a device list, the device key must be on it. With no list, the
	// AEAD is the check: only a holder of the keys made the blob.
	if len(list) > 0 && !slices.Contains(list, env.Device) {
		return store.Result{}, fmt.Errorf("%w: %s", errNotListed, env.Device.Hex())
	}
	return s.Store.Save(env.Record)
}

func isSelf(kind nostr.Kind) bool { return slices.Contains(Kinds, kind) }

// Local says if an event is self data that stays in the local store: a
// draft, a checkpoint, a part, or a deletion of them. A device never sends
// it to a relay. It goes only to storage citizens.
func Local(event nostr.Event) bool {
	switch event.Kind {
	case draft.Kind, draft.CheckpointKind, draft.PartKind:
		return true
	case nostr.KindDeletion:
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "k" && slices.Contains([]string{"31234", "1234", "3275"}, tag[1]) {
				return true
			}
		}
	}
	return false
}
