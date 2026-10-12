package selfsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/store"
)

// learnMax is the most blobs that one sync gets again to learn if their
// record is replaced or deleted (docs/storage/SPEC.md, section 4.7).
const learnMax = 1024

// replaced says what replaced or deleted the record of one blob. The store
// of the device holds that record.
type replaced struct {
	// Kind and D name a replaceable or addressable record: the store holds
	// a newer version of it.
	Kind nostr.Kind `json:"k,omitempty"`
	D    string     `json:"d,omitempty"`
	// By is the ID of the deletion request that names the record.
	By string `json:"by,omitempty"`
}

// oldState is what a device knows of the blobs whose record its store does
// not keep. It is shared by the storage citizens, in old.json.
type oldState struct {
	V int `json:"v"`
	// Old holds each listed blob whose record the store replaced or
	// deleted. Repair deletes its pieces.
	Old map[string]replaced `json:"old"`
	// Checked holds each listed blob that the device got, and that its
	// store did not keep for another reason. The device does not get it
	// again to learn about it.
	Checked map[string]bool `json:"checked,omitempty"`
}

func (s *Syncer) oldPath() string { return filepath.Join(s.State, "old.json") }

func (s *Syncer) loadOld() (oldState, error) {
	st := oldState{V: 1, Old: map[string]replaced{}, Checked: map[string]bool{}}
	raw, err := os.ReadFile(s.oldPath())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("%s: %w", s.oldPath(), err)
	}
	if st.Old == nil {
		st.Old = map[string]replaced{}
	}
	if st.Checked == nil {
		st.Checked = map[string]bool{}
	}
	return st, nil
}

// classify notes what the store did with the record of a blob that the
// device got. A record that the store keeps needs no note.
func (r *run) classify(id string, record nostr.Event, result store.Result) error {
	switch {
	case result.Outcome == store.Superseded:
		r.olds.Old[id] = replaced{Kind: record.Kind, D: record.Tags.GetD()}
	case result.Outcome == store.Refused && result.Reason == store.ReasonDeleted:
		by, err := r.deletionOf(record)
		if err != nil {
			return err
		}
		if by == "" {
			r.olds.Checked[id] = true
			return nil
		}
		r.olds.Old[id] = replaced{By: by}
	case result.Outcome == store.Refused:
		r.olds.Checked[id] = true
	}
	return nil
}

// deletionOf returns the ID of a deletion request in the store that names
// the record: by its ID, or by the coordinate of an addressable record.
func (r *run) deletionOf(record nostr.Event) (string, error) {
	filters := []nostr.Filter{{Kinds: []nostr.Kind{nostr.KindDeletion}, Authors: []nostr.PubKey{r.s.Citizen},
		Tags: nostr.TagMap{"e": {record.ID.Hex()}}}}
	if record.Kind.IsAddressable() {
		filters = append(filters, nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Authors: []nostr.PubKey{r.s.Citizen},
			Tags: nostr.TagMap{"a": {fmt.Sprintf("%d:%s:%s", record.Kind, record.PubKey.Hex(), record.Tags.GetD())}}, Since: record.CreatedAt})
	}
	for _, f := range filters {
		found, err := r.s.Store.Query(f)
		if err != nil {
			return "", err
		}
		if len(found) > 0 {
			return found[0].ID.Hex(), nil
		}
	}
	return "", nil
}

// deleted notes the blob of each record that a deletion request in the
// store names by its ID, and that the store does not keep. The device
// learns these with no get.
func (r *run) deleted(records []nostr.Event) {
	held := make(map[nostr.ID]bool, len(records))
	for _, e := range records {
		held[e.ID] = true
	}
	for _, e := range records {
		if e.Kind != nostr.KindDeletion {
			continue
		}
		for _, tag := range e.Tags {
			if len(tag) < 2 || tag[0] != "e" {
				continue
			}
			named, err := nostr.IDFromHex(tag[1])
			if err != nil || held[named] {
				continue
			}
			if id := r.current.BlobID(named); r.blobs[id] != nil {
				r.olds.Old[id] = replaced{By: e.ID.Hex()}
			}
		}
	}
}

// replacer returns the record that replaced or deleted the record of an
// old blob, if the store holds it.
func (r *run) replacer(o replaced) (*nostr.Event, error) {
	f := nostr.Filter{Authors: []nostr.PubKey{r.s.Citizen}}
	if o.By != "" {
		id, err := nostr.IDFromHex(o.By)
		if err != nil {
			return nil, nil
		}
		f.IDs = []nostr.ID{id}
	} else {
		f.Kinds = []nostr.Kind{o.Kind}
		if o.Kind.IsAddressable() {
			f.Tags = nostr.TagMap{"d": {o.D}}
		}
	}
	found, err := r.s.Store.Query(f)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return &found[0], nil
}

// onStorage says that the storage citizens hold the blob of a record,
// from their listings: k pieces of one seal, or a whole blob of format 1.
func (r *run) onStorage(event nostr.Event) bool {
	m := r.blobs[r.current.BlobID(event.ID)]
	return m != nil && (len(m.old) > 0 || r.best(m, false) != nil)
}

// learn gets each listed blob that the device got before this version,
// whose record its store does not keep, and that it knows nothing of. The
// store says if a newer version replaced the record, or a deletion named
// it. Only a sync that repairs learns, at most learnMax blobs.
func (r *run) learn(ctx context.Context, local map[string]bool) error {
	var ids []string
	for id := range r.blobs {
		_, old := r.olds.Old[id]
		if r.seen[id] && !local[id] && !old && !r.olds.Checked[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	ids = ids[:min(len(ids), learnMax)]
	for chunk := range slices.Chunk(ids, getChunk) {
		all, _ := r.rebuildable(chunk)
		if err := r.fetch(ctx, all); err != nil {
			return err
		}
		for _, b := range all {
			if b.blob == nil {
				continue
			}
			if err := r.take(ctx, b.id, b.blob, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// prune forgets each old or checked blob that no storage citizen lists.
func (r *run) prune() {
	for id := range r.olds.Old {
		if r.blobs[id] == nil {
			delete(r.olds.Old, id)
		}
	}
	for id := range r.olds.Checked {
		if r.blobs[id] == nil {
			delete(r.olds.Checked, id)
		}
	}
}
