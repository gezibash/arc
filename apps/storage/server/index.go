package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gezibash/arc/sdk/atomicfile"
	wire "github.com/gezibash/arc/sdk/storage"
	bolt "go.etcd.io/bbolt"
)

// The index of an account is accounts/<account>/index.db, a bbolt file
// that only the service opens. Its buckets:
//
//	meta        seq, used and count: the last sequence number, the bytes
//	            in use and the number of blobs; imported marks the import
//	            of the entries of an older state.json
//	blobs       ID → seq, size and time of the put
//	tombstones  ID → seq and time of the delete
//	changes     seq → ID: the last change of each ID, for list
//
// A put or a delete changes a few keys in one transaction. It does not
// write the whole index.
var (
	bucketMeta       = []byte("meta")
	bucketBlobs      = []byte("blobs")
	bucketTombstones = []byte("tombstones")
	bucketChanges    = []byte("changes")

	keySeq      = []byte("seq")
	keyUsed     = []byte("used")
	keyCount    = []byte("count")
	keyImported = []byte("imported")
)

// index is the index of one account in one transaction.
type index struct{ tx *bolt.Tx }

func u64(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func put64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func (x index) meta(key []byte) uint64 { return u64(x.tx.Bucket(bucketMeta).Get(key)) }

func (x index) setMeta(key []byte, v uint64) error {
	return x.tx.Bucket(bucketMeta).Put(key, put64(v))
}

func (x index) seq() uint64  { return x.meta(keySeq) }
func (x index) used() int64  { return int64(x.meta(keyUsed)) }
func (x index) count() int64 { return int64(x.meta(keyCount)) }

// blob returns the entry of a blob.
func (x index) blob(id string) (blob, bool) {
	v := x.tx.Bucket(bucketBlobs).Get([]byte(id))
	if len(v) != 24 {
		return blob{}, false
	}
	return blob{Seq: u64(v), Size: int64(u64(v[8:])), Put: time.Unix(0, int64(u64(v[16:])))}, true
}

func (x index) tombstone(id string) (tombstone, bool) {
	v := x.tx.Bucket(bucketTombstones).Get([]byte(id))
	if len(v) != 16 {
		return tombstone{}, false
	}
	return tombstone{Seq: u64(v), Deleted: time.Unix(0, int64(u64(v[8:])))}, true
}

// status returns StatusHeld or StatusTombstoned for an ID that takes no
// put, or "".
func (x index) status(id string, now time.Time) string {
	if _, ok := x.blob(id); ok {
		return wire.StatusHeld
	}
	if t, ok := x.tombstone(id); ok && now.Sub(t.Deleted) < TombstoneAge {
		return wire.StatusTombstoned
	}
	return ""
}

func (x index) setBlob(id string, b blob) error {
	v := binary.BigEndian.AppendUint64(nil, b.Seq)
	v = binary.BigEndian.AppendUint64(v, uint64(b.Size))
	v = binary.BigEndian.AppendUint64(v, uint64(b.Put.UnixNano()))
	if err := x.tx.Bucket(bucketBlobs).Put([]byte(id), v); err != nil {
		return err
	}
	return x.tx.Bucket(bucketChanges).Put(put64(b.Seq), []byte(id))
}

func (x index) setTombstone(id string, t tombstone) error {
	v := binary.BigEndian.AppendUint64(nil, t.Seq)
	v = binary.BigEndian.AppendUint64(v, uint64(t.Deleted.UnixNano()))
	if err := x.tx.Bucket(bucketTombstones).Put([]byte(id), v); err != nil {
		return err
	}
	return x.tx.Bucket(bucketChanges).Put(put64(t.Seq), []byte(id))
}

// add records a blob, and removes its tombstone. It returns the sequence
// number of the change.
func (x index) add(id string, size int64, now time.Time) (uint64, error) {
	if t, ok := x.tombstone(id); ok {
		if err := x.tx.Bucket(bucketChanges).Delete(put64(t.Seq)); err != nil {
			return 0, err
		}
		if err := x.tx.Bucket(bucketTombstones).Delete([]byte(id)); err != nil {
			return 0, err
		}
	}
	seq := x.seq() + 1
	if err := x.setBlob(id, blob{Seq: seq, Size: size, Put: now}); err != nil {
		return 0, err
	}
	return seq, errors.Join(x.setMeta(keySeq, seq), x.setMeta(keyUsed, uint64(x.used()+size)), x.setMeta(keyCount, uint64(x.count()+1)))
}

// remove deletes the entry of a blob, and keeps its ID as a tombstone. It
// returns false if the account holds no blob with the ID.
func (x index) remove(id string, now time.Time) (uint64, bool, error) {
	b, ok := x.blob(id)
	if !ok {
		return 0, false, nil
	}
	if err := x.tx.Bucket(bucketChanges).Delete(put64(b.Seq)); err != nil {
		return 0, false, err
	}
	if err := x.tx.Bucket(bucketBlobs).Delete([]byte(id)); err != nil {
		return 0, false, err
	}
	seq := x.seq() + 1
	if err := x.setTombstone(id, tombstone{Seq: seq, Deleted: now}); err != nil {
		return 0, false, err
	}
	return seq, true, errors.Join(x.setMeta(keySeq, seq), x.setMeta(keyUsed, uint64(x.used()-b.Size)), x.setMeta(keyCount, uint64(x.count()-1)))
}

// list returns the entries with a sequence number after the cursor, in
// order, and at most limit of them.
func (x index) list(after uint64, limit int) []wire.Entry {
	var entries []wire.Entry
	c := x.tx.Bucket(bucketChanges).Cursor()
	for k, v := c.Seek(put64(after + 1)); k != nil && len(entries) < limit; k, v = c.Next() {
		id := string(v)
		if b, ok := x.blob(id); ok && b.Seq == u64(k) {
			entries = append(entries, wire.Entry{Seq: b.Seq, ID: id, Size: b.Size})
		} else {
			entries = append(entries, wire.Entry{Seq: u64(k), ID: id, Deleted: true})
		}
	}
	return entries
}

// importOld copies the entries of an older state.json into the index.
func (x index) importOld(a *account) error {
	for id, b := range a.Blobs {
		if err := x.setBlob(id, b); err != nil {
			return err
		}
	}
	for id, t := range a.Tombstones {
		if err := x.setTombstone(id, t); err != nil {
			return err
		}
	}
	return errors.Join(x.setMeta(keySeq, a.Seq), x.setMeta(keyUsed, uint64(a.Used)),
		x.setMeta(keyCount, uint64(len(a.Blobs))), x.tx.Bucket(bucketMeta).Put(keyImported, []byte{1}))
}

// open returns the open index of an account. The first open of an account
// makes its index. If state.json holds the entries of an older app, the
// open imports them in one transaction, and then removes them from
// state.json. The lock of state.json covers the import, so an operator
// command does not write the old entries back. If the program stops
// before state.json is written, the next open sees the mark imported, and
// only writes state.json.
func (s *store) open(name string) (*bolt.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := s.dbs[name]; db != nil {
		return db, nil
	}
	if s.dbs == nil {
		s.dbs = map[string]*bolt.DB{}
	}
	unlock, err := atomicfile.Lock(s.statePath(name))
	if err != nil {
		return nil, err
	}
	defer unlock()
	db, err := bolt.Open(s.indexPath(name), 0o600, &bolt.Options{Timeout: 2 * time.Second, PageSize: 4096})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.indexPath(name), err)
	}
	a, err := s.load(name)
	if err == nil {
		err = db.Update(func(tx *bolt.Tx) error {
			for _, b := range [][]byte{bucketMeta, bucketBlobs, bucketTombstones, bucketChanges} {
				if _, err := tx.CreateBucketIfNotExists(b); err != nil {
					return err
				}
			}
			if tx.Bucket(bucketMeta).Get(keyImported) != nil {
				return nil
			}
			return index{tx}.importOld(a)
		})
	}
	if err == nil && a.old() {
		a.Seq, a.Used, a.Blobs, a.Tombstones = 0, 0, nil, nil
		err = s.write(name, a)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.dbs[name] = db
	return db, nil
}

// view runs f on the index of an account.
func (s *store) view(name string, f func(index) error) error {
	db, err := s.open(name)
	if err != nil {
		return err
	}
	return db.View(func(tx *bolt.Tx) error { return f(index{tx}) })
}

// update runs f on the index of an account in one transaction.
func (s *store) update(name string, f func(index) error) error {
	db, err := s.open(name)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error { return f(index{tx}) })
}

// openAll opens the index of each account with a directory, so that each
// older state.json is imported at the start of the service. It returns the
// count of accounts.
func (s *store) openAll() (int, error) {
	entries, err := os.ReadDir(s.accountsDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !wire.IsHex64(e.Name()) {
			continue
		}
		if _, err := s.open(e.Name()); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// close closes each open index.
func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for name, db := range s.dbs {
		errs = append(errs, db.Close())
		delete(s.dbs, name)
	}
	return errors.Join(errs...)
}
