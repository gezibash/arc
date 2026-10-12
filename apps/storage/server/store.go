package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gezibash/arc/sdk/atomicfile"
	wire "github.com/gezibash/arc/sdk/storage"
	bolt "go.etcd.io/bbolt"
)

// TombstoneAge is how long the app refuses a put of a deleted ID.
const TombstoneAge = 90 * 24 * time.Hour

// store keeps the blobs of each account under its own directory:
//
//	accounts/<account>/state.json   the record and the grant set
//	accounts/<account>/index.db     the index: the cursor and the entries
//	accounts/<account>/blobs/<id>   the bytes of one blob
//
// The name of an account is its account key, in hex.
//
// The lock of state.json covers each change of state.json, so the service
// and an operator command do not lose a change. Only the service opens
// index.db. A bbolt transaction covers each change of the index.
type store struct {
	dir string
	// owners are the account keys of STORAGE_OWNERS. Each is admitted at
	// quota until the operator gives it a record.
	owners []string
	quota  int64
	now    func() time.Time

	mu  sync.Mutex
	dbs map[string]*bolt.DB
}

// account is the content of state.json.
type account struct {
	// Record is the admission of the account: its status, quota, plan and
	// end. See admission.go.
	Record *record `json:"record,omitempty"`
	// Devices are the device keys of the newest grant set. Each can use
	// the account. A device key that the set does not hold has no grant.
	Devices []string `json:"devices,omitempty"`
	// GrantsAt is the time of the newest grant set, in Unix seconds.
	GrantsAt int64 `json:"grants_at,omitempty"`
	// Draining is the time at which the owner started the drain of the
	// account, in Unix seconds, or 0. A draining account takes no put.
	Draining int64 `json:"draining,omitempty"`

	// Seq, Used, Blobs and Tombstones are the index of an older app. The
	// service imports them into index.db once, and then removes them. An
	// operator command keeps them as they are.
	Seq        uint64               `json:"seq,omitempty"`
	Used       int64                `json:"used,omitempty"`
	Blobs      map[string]blob      `json:"blobs,omitempty"`
	Tombstones map[string]tombstone `json:"tombstones,omitempty"`
}

// old says if state.json holds the index of an older app.
func (a *account) old() bool {
	return a.Seq != 0 || a.Used != 0 || len(a.Blobs) > 0 || len(a.Tombstones) > 0
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

func (s *store) accountsDir() string { return filepath.Join(s.dir, "accounts") }

func (s *store) accountDir(name string) string { return filepath.Join(s.accountsDir(), name) }

func (s *store) statePath(name string) string { return filepath.Join(s.accountDir(name), "state.json") }

func (s *store) indexPath(name string) string { return filepath.Join(s.accountDir(name), "index.db") }

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
	return s.write(name, a)
}

// write writes state.json. The caller holds its lock.
func (s *store) write(name string, a *account) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.statePath(name), raw, 0o600)
}

func (s *store) load(name string) (*account, error) {
	a := &account{}
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
	return a, nil
}

// used returns the bytes in use of an account. An account with no
// directory uses none, and gets no index.
func (s *store) used(name string) (int64, error) {
	if _, err := os.Stat(s.accountDir(name)); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	var used int64
	err := s.view(name, func(x index) error {
		used = x.used()
		return nil
	})
	return used, err
}

// granted says if the newest grant set of an account holds a device key.
func (s *store) granted(name, device string) (bool, error) {
	a, err := s.load(name)
	if err != nil {
		return false, err
	}
	return slices.Contains(a.Devices, device), nil
}

// grant keeps a grant set of an account, unless the account holds a newer
// set. A set of the same time replaces the set, so a signer can send it
// again.
func (s *store) grant(name string, devices []string, at int64) error {
	return s.change(name, func(a *account) (bool, error) {
		if at < a.GrantsAt {
			return false, refusal{wire.ErrStaleGrants, fmt.Sprintf("the account holds a grant set of %d, which is newer than %d", a.GrantsAt, at)}
		}
		a.Devices = slices.Clone(devices)
		a.GrantsAt = at
		return true, nil
	})
}

// refusal is an answer of the store to a request.
type refusal struct{ code, message string }

func (r refusal) Error() string { return r.message }

// admit checks a put before its bytes go. reserved is the bytes of the
// puts of the session that are not in the index yet. It returns
// StatusSend, or a status for which no bytes go.
func (s *store) admit(name, id string, size, reserved int64) (string, error) {
	if !wire.GoodSize(size) {
		return "", refusal{wire.ErrBadSize, fmt.Sprintf("a blob has a size that is a power of two from %d to %d bytes, not %d", wire.MinBlob, wire.MaxBlob, size)}
	}
	a, err := s.load(name)
	if err != nil {
		return "", err
	}
	var status string
	var used int64
	err = s.view(name, func(x index) error {
		status, used = x.status(id, s.now()), x.used()
		return nil
	})
	if err != nil || status != "" {
		return status, err
	}
	return wire.StatusSend, s.room(name, a, used+reserved, size)
}

// room refuses a put on a closing account, and a put that passes the
// quota of the account.
func (s *store) room(name string, a *account, used, size int64) error {
	v := s.standing(name, a)
	if v.state == wire.StateDraining {
		return refusal{wire.ErrRefused, "the account is draining: it takes no put, and keeps each read, list and delete"}
	}
	if v.state != wire.StateOpen {
		return refusal{wire.ErrRefused, fmt.Sprintf("the account is %s: it takes no put, and its reads end at %s", v.state, time.Unix(v.readsEnd, 0).UTC().Format(time.DateOnly))}
	}
	if used+size <= v.quota {
		return nil
	}
	return refusal{wire.ErrOverQuota, fmt.Sprintf("over quota: the account uses %d of %d bytes, and this blob needs %d more", used, v.quota, size)}
}

// writeBlob writes the bytes of a blob to its file. The directory is
// synced later, by commit, once for each group of puts.
func (s *store) writeBlob(name, id string, data []byte) error {
	dir := filepath.Dir(s.blobPath(name, id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+id+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.blobPath(name, id))
}

// written is a put whose bytes are in their file, and not in the index.
type written struct {
	n    uint64
	id   string
	size int64
}

// stored is the result of one put of a commit.
type stored struct {
	status string
	seq    uint64
	err    error
}

// beforeIndex runs after the files of a commit, and before its index
// transaction. A test stops the program there.
var beforeIndex = func() {}

// commit records a group of puts whose bytes are in their files: it syncs
// the directory of the files, and then writes each entry in one
// transaction. It checks each put again, because another session of the
// account can put at the same time. A put that this check refuses loses
// its file.
//
// The bytes go first. If the program stops before the transaction ends,
// each file stays with no entry. The app does not serve it, and the next
// put of the ID replaces it.
func (s *store) commit(name string, puts []written) ([]stored, error) {
	results := make([]stored, len(puts))
	dir, err := os.Open(filepath.Dir(s.blobPath(name, "x")))
	if err != nil {
		return results, err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return results, err
	}
	beforeIndex()
	a, err := s.load(name)
	if err != nil {
		return results, err
	}
	var lose []string
	err = s.update(name, func(x index) error {
		lose = lose[:0]
		now := s.now()
		for i, p := range puts {
			if status := x.status(p.id, now); status != "" {
				// A tombstoned ID holds no file. A held ID keeps the file,
				// which holds another sealing of the same record.
				if status == wire.StatusTombstoned {
					lose = append(lose, p.id)
				}
				results[i] = stored{status: status}
				continue
			}
			if err := s.room(name, a, x.used(), p.size); err != nil {
				lose = append(lose, p.id)
				results[i] = stored{err: err}
				continue
			}
			seq, err := x.add(p.id, p.size, now)
			if err != nil {
				return err
			}
			results[i] = stored{status: wire.StatusStored, seq: seq}
		}
		return nil
	})
	if err != nil {
		return make([]stored, len(puts)), err
	}
	for _, id := range lose {
		if err := os.Remove(s.blobPath(name, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return results, err
		}
	}
	return results, nil
}

// put writes the bytes of one blob, and then records it.
func (s *store) put(name, id string, data []byte) (status string, seq uint64, err error) {
	if status, err := s.admit(name, id, int64(len(data)), 0); err != nil || status != wire.StatusSend {
		return status, 0, err
	}
	if err := s.writeBlob(name, id, data); err != nil {
		return "", 0, err
	}
	results, err := s.commit(name, []written{{id: id, size: int64(len(data))}})
	if err != nil {
		return "", 0, err
	}
	return results[0].status, results[0].seq, results[0].err
}

// sizes returns the size of each blob, or the code not_found.
func (s *store) sizes(name string, ids []string) ([]wire.Item, error) {
	items := make([]wire.Item, len(ids))
	err := s.view(name, func(x index) error {
		for i, id := range ids {
			items[i].ID = id
			if b, ok := x.blob(id); ok {
				items[i].Size = b.Size
			} else {
				items[i].Error = wire.ErrNotFound
			}
		}
		return nil
	})
	return items, err
}

// get reads the bytes of a blob.
func (s *store) get(name, id string) ([]byte, error) {
	items, err := s.sizes(name, []string{id})
	if err != nil {
		return nil, err
	}
	if items[0].Error != "" {
		return nil, refusal{wire.ErrNotFound, "the account holds no blob " + id}
	}
	return os.ReadFile(s.blobPath(name, id))
}

// remove deletes blobs in one transaction, and keeps each ID as a
// tombstone. The index goes first, and then the files. A file with no
// entry is not served, and the next put of the ID replaces it.
func (s *store) remove(name string, ids []string) ([]wire.Item, error) {
	items := make([]wire.Item, len(ids))
	err := s.update(name, func(x index) error {
		now := s.now()
		for i, id := range ids {
			seq, ok, err := x.remove(id, now)
			if err != nil {
				return err
			}
			items[i] = wire.Item{ID: id, Status: wire.StatusAbsent}
			if ok {
				items[i] = wire.Item{ID: id, Status: wire.StatusDeleted, Seq: seq}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Status != wire.StatusDeleted {
			continue
		}
		if err := os.Remove(s.blobPath(name, item.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return items, err
		}
	}
	return items, nil
}

// list returns the entries with a sequence number after the cursor, in
// order, and at most limit of them. Each ID has one entry: its last change.
func (s *store) list(name string, after uint64, limit int) ([]wire.Entry, uint64, error) {
	var entries []wire.Entry
	err := s.view(name, func(x index) error {
		entries = x.list(after, limit)
		return nil
	})
	next := after
	if len(entries) > 0 {
		next = entries[len(entries)-1].Seq
	}
	return entries, next, err
}

// usage returns the bytes in use, the number of blobs, and the record of
// the account.
func (s *store) usage(name string) (wire.Reply, error) {
	a, err := s.load(name)
	if err != nil {
		return wire.Reply{}, err
	}
	var used, count int64
	err = s.view(name, func(x index) error {
		used, count = x.used(), x.count()
		return nil
	})
	if err != nil {
		return wire.Reply{}, err
	}
	v := s.standing(name, a)
	return wire.Reply{Used: used, Quota: v.quota, Blobs: int(count), State: v.state, Plan: v.plan, Expires: v.expires}, nil
}

// audit answers an audit: the size and the AuditProof of each blob for
// the nonce, or the code not_found. A blob whose file is gone is
// not_found, as a blob with no entry is. The app reads each file whole for
// each nonce, and keeps no proof.
func (s *store) audit(name string, ids []string, nonce []byte) ([]wire.Item, error) {
	items, err := s.sizes(name, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Error != "" {
			continue
		}
		proof, err := s.proof(name, items[i].ID, nonce)
		if errors.Is(err, os.ErrNotExist) {
			items[i] = wire.Item{ID: items[i].ID, Error: wire.ErrNotFound}
			continue
		}
		if err != nil {
			return nil, err
		}
		items[i].Proof = proof
	}
	return items, nil
}

// proof hashes the file of one blob for an audit.
func (s *store) proof(name, id string, nonce []byte) (string, error) {
	f, err := os.Open(s.blobPath(name, id))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := wire.AuditHash(nonce, id)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// drain sets the drain of an account, or ends it with off, and returns
// the usage of the account.
func (s *store) drain(name string, off bool) (wire.Reply, error) {
	err := s.change(name, func(a *account) (bool, error) {
		if off == (a.Draining == 0) {
			return false, nil
		}
		a.Draining = 0
		if !off {
			a.Draining = s.now().Unix()
		}
		return true, nil
	})
	if err != nil {
		return wire.Reply{}, err
	}
	return s.usage(name)
}
