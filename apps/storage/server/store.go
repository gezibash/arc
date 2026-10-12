package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/gezibash/arc/sdk/atomicfile"
	wire "github.com/gezibash/arc/sdk/storage"
)

// TombstoneAge is how long the app refuses a put of a deleted ID.
const TombstoneAge = 90 * 24 * time.Hour

// store keeps the blobs of each account under its own directory:
//
//	accounts/<account>/state.json   the grants, the cursor and the entries
//	accounts/<account>/blobs/<id>   the bytes of one blob
//
// Each write is atomic. The lock of the account covers each change of
// state.json, so two sessions of one account do not lose a change.
type store struct {
	dir   string
	quota int64
	now   func() time.Time
}

// account is the content of state.json.
type account struct {
	// Grants are the keys that can use the account. The first key that
	// opens the account gets the first grant. A later step adds grants
	// for device keys, which the account key signs; see
	// docs/proposals/storage.md, section 2.3.
	Grants []string `json:"grants"`
	// Seq is the sequence number of the last change.
	Seq uint64 `json:"seq"`
	// Used is the sum of the sizes of the blobs.
	Used  int64           `json:"used"`
	Blobs map[string]blob `json:"blobs"`
	// Tombstones are the IDs that the owner deleted.
	Tombstones map[string]tombstone `json:"tombstones"`
}

type blob struct {
	Seq  uint64    `json:"seq"`
	Size int64     `json:"size"`
	Put  time.Time `json:"put"`
}

type tombstone struct {
	Seq     uint64    `json:"seq"`
	Deleted time.Time `json:"deleted"`
}

// errNoGrant says that a key has no grant in an account.
var errNoGrant = errors.New("no grant")

func (s *store) accountDir(name string) string { return filepath.Join(s.dir, "accounts", name) }

func (s *store) statePath(name string) string { return filepath.Join(s.accountDir(name), "state.json") }

func (s *store) blobPath(name, id string) string {
	return filepath.Join(s.accountDir(name), "blobs", id)
}

// change runs f on the state of an account under its lock, and writes the
// state if f returns true.
func (s *store) change(name string, f func(*account) (bool, error)) error {
	unlock, err := atomicfile.Lock(s.statePath(name))
	if err != nil {
		return err
	}
	defer unlock()
	a, err := s.load(name)
	if err != nil {
		return err
	}
	write, err := f(a)
	if err != nil || !write {
		return err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.statePath(name), raw, 0o600)
}

func (s *store) load(name string) (*account, error) {
	a := &account{Blobs: map[string]blob{}, Tombstones: map[string]tombstone{}}
	raw, err := os.ReadFile(s.statePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, a); err != nil {
		return nil, fmt.Errorf("%s: %w", s.statePath(name), err)
	}
	if a.Blobs == nil {
		a.Blobs = map[string]blob{}
	}
	if a.Tombstones == nil {
		a.Tombstones = map[string]tombstone{}
	}
	return a, nil
}

// authorize lets key use an account. A new account takes key as its first
// grant. An account with grants refuses another key with errNoGrant.
func (s *store) authorize(name, key string) error {
	return s.change(name, func(a *account) (bool, error) {
		if slices.Contains(a.Grants, key) {
			return false, nil
		}
		if len(a.Grants) > 0 {
			return false, errNoGrant
		}
		a.Grants = []string{key}
		return true, nil
	})
}

// refusal is an answer of the store to a request.
type refusal struct{ code, message string }

func (r refusal) Error() string { return r.message }

// admit checks a put before its bytes go. It returns StatusSend, or a
// status for which no bytes go.
func (s *store) admit(name, id string, size int64) (string, error) {
	if !wire.GoodSize(size) {
		return "", refusal{wire.ErrBadSize, fmt.Sprintf("a blob has a size that is a power of two from %d to %d bytes, not %d", wire.MinBlob, wire.MaxBlob, size)}
	}
	a, err := s.load(name)
	if err != nil {
		return "", err
	}
	if status := heldOrTombstoned(a, id, s.now()); status != "" {
		return status, nil
	}
	return wire.StatusSend, overQuota(a, size, s.quota)
}

func heldOrTombstoned(a *account, id string, now time.Time) string {
	if _, ok := a.Blobs[id]; ok {
		return wire.StatusHeld
	}
	if t, ok := a.Tombstones[id]; ok && now.Sub(t.Deleted) < TombstoneAge {
		return wire.StatusTombstoned
	}
	return ""
}

func overQuota(a *account, size, quota int64) error {
	if a.Used+size <= quota {
		return nil
	}
	return refusal{wire.ErrOverQuota, fmt.Sprintf("over quota: the account uses %d of %d bytes, and this blob needs %d more", a.Used, quota, size)}
}

// put writes the bytes of a blob, and then records it. It checks the
// quota again, because another session of the account can put at the same
// time.
func (s *store) put(name, id string, data []byte) (status string, seq uint64, err error) {
	err = s.change(name, func(a *account) (bool, error) {
		now := s.now()
		if status = heldOrTombstoned(a, id, now); status != "" {
			return false, nil
		}
		if err := overQuota(a, int64(len(data)), s.quota); err != nil {
			return false, err
		}
		// The bytes go first. If the program stops before the state, the
		// file stays with no entry, and the next put of the ID replaces it.
		if err := atomicfile.Write(s.blobPath(name, id), data, 0o600); err != nil {
			return false, err
		}
		delete(a.Tombstones, id)
		a.Seq++
		a.Used += int64(len(data))
		a.Blobs[id] = blob{Seq: a.Seq, Size: int64(len(data)), Put: now}
		status, seq = wire.StatusStored, a.Seq
		return true, nil
	})
	return status, seq, err
}

// get reads the bytes of a blob.
func (s *store) get(name, id string) ([]byte, error) {
	a, err := s.load(name)
	if err != nil {
		return nil, err
	}
	if _, ok := a.Blobs[id]; !ok {
		return nil, refusal{wire.ErrNotFound, "the account holds no blob " + id}
	}
	return os.ReadFile(s.blobPath(name, id))
}

// remove deletes a blob, and keeps its ID as a tombstone.
func (s *store) remove(name, id string) (status string, seq uint64, err error) {
	err = s.change(name, func(a *account) (bool, error) {
		b, ok := a.Blobs[id]
		if !ok {
			status = wire.StatusAbsent
			return false, nil
		}
		// The state goes first. A file with no entry is not served, and the
		// next put of the ID replaces it.
		a.Seq++
		a.Used -= b.Size
		delete(a.Blobs, id)
		a.Tombstones[id] = tombstone{Seq: a.Seq, Deleted: s.now()}
		status, seq = wire.StatusDeleted, a.Seq
		return true, nil
	})
	if err == nil && status == wire.StatusDeleted {
		if rmErr := os.Remove(s.blobPath(name, id)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = rmErr
		}
	}
	return status, seq, err
}

// list returns the entries with a sequence number after the cursor, in
// order, and at most limit of them. Each ID has one entry: its last change.
func (s *store) list(name string, after uint64, limit int) ([]wire.Entry, uint64, error) {
	a, err := s.load(name)
	if err != nil {
		return nil, 0, err
	}
	var entries []wire.Entry
	for id, b := range a.Blobs {
		if b.Seq > after {
			entries = append(entries, wire.Entry{Seq: b.Seq, ID: id, Size: b.Size})
		}
	}
	for id, t := range a.Tombstones {
		if t.Seq > after {
			entries = append(entries, wire.Entry{Seq: t.Seq, ID: id, Deleted: true})
		}
	}
	slices.SortFunc(entries, func(x, y wire.Entry) int { return cmp.Compare(x.Seq, y.Seq) })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	next := after
	if len(entries) > 0 {
		next = entries[len(entries)-1].Seq
	}
	return entries, next, nil
}

// usage returns the bytes in use and the number of blobs.
func (s *store) usage(name string) (int64, int, error) {
	a, err := s.load(name)
	if err != nil {
		return 0, 0, err
	}
	return a.Used, len(a.Blobs), nil
}
