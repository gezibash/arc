package citizen

import (
	"context"
	"encoding/json"
	"sync"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/store"
)

// listBucket keeps the device list that this machine last opened, sealed
// with NIP-44 from the cache key to itself, as the data roots are. A device
// thus checks the signer of self data with no path to the signer.
var (
	listBucket = []byte("device-list")
	listKey    = []byte("list")
)

// cachedList is the device list on the disk. Exists is false when the
// citizen had no device list at all.
type cachedList struct {
	Exists  bool                 `json:"exists"`
	At      nostr.Timestamp      `json:"at"`
	Devices []devices.Device     `json:"devices"`
	Revoked []devices.Revocation `json:"revoked"`
}

// signers answers the store from the device list that this machine last
// opened. The store asks it while it holds the store, so it reads only
// memory.
type signers struct {
	citizen, own nostr.PubKey
	mu           sync.RWMutex
	known        bool
	cached       cachedList
}

func (s *signers) Standing(citizen, signer nostr.PubKey) store.Standing {
	if citizen != s.citizen {
		return store.Standing{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	l := devices.List{Devices: s.cached.Devices, Revoked: s.cached.Revoked}
	if r, ok := l.RevokedAt(signer); ok {
		return store.Standing{RevokedAt: r.At, RevokedGen: r.Gen}
	}
	if signer == s.own || l.Has(signer) {
		return store.Standing{Listed: true}
	}
	return store.Standing{NoList: s.known && !s.cached.Exists}
}

func (s *signers) set(c cachedList) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known, s.cached = true, c
}

func (s *signers) at() (nostr.Timestamp, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cached.At, s.known && s.cached.Exists
}

func (s *signers) list() devices.List {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return devices.List{Devices: s.cached.Devices, Revoked: s.cached.Revoked, At: s.cached.At}
}

// loadSigners reads the cached device list, and gives it to the store. A
// machine with no device key keeps no cache, and checks no device key.
func (sess *Session) loadSigners(ctx context.Context) {
	sess.signers = &signers{citizen: sess.Key.Public, own: sess.Key.Public}
	if sess.Remote {
		sess.signers.own = sess.Device.Public
	}
	sess.store.SetSigners(sess.signers)
	k, err := sess.cacheKey()
	if err != nil {
		return
	}
	var sealed []byte
	_ = sess.roots.View(func(tx kv.Tx) error {
		if b := tx.Bucket(listBucket); b != nil {
			sealed = b.Get(listKey)
		}
		return nil
	})
	if sealed == nil {
		return
	}
	text, err := k.Decrypt(ctx, string(sealed), k.Public)
	if err != nil {
		sess.warnf("arc: the device list on this disk does not open: %v\n", err)
		return
	}
	var c cachedList
	if err := json.Unmarshal([]byte(text), &c); err != nil {
		sess.warnf("arc: the device list on this disk is not valid: %v\n", err)
		return
	}
	sess.signers.set(c)
}

// keepList remembers a device list that this machine opened, in memory and
// on its disk.
func (sess *Session) keepList(ctx context.Context, l devices.List, exists bool) error {
	c := cachedList{Exists: exists, At: l.At, Devices: l.Devices, Revoked: l.Revoked}
	sess.signers.set(c)
	k, err := sess.cacheKey()
	if err != nil {
		return err
	}
	text, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := k.Encrypt(ctx, string(text), k.Public)
	if err != nil {
		return err
	}
	return sess.roots.Update(func(tx kv.Tx) error {
		b, err := tx.CreateBucketIfNotExists(listBucket)
		if err != nil {
			return err
		}
		return b.Put(listKey, []byte(sealed))
	})
}
