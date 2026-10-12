package citizen

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/node"
)

// rootsBucket keeps each data root that this machine holds, by generation.
// Each value is the root as hex, sealed with NIP-44 from the cache key to
// itself. Thus a copy of the file alone opens no data.
var rootsBucket = []byte("data-roots")

// openedBucket keeps the ID of the roots event that this machine opened
// last, under openedKey.
var (
	openedBucket = []byte("roots-event")
	openedKey    = []byte("opened")
)

// DataRoot returns the newest data root that this machine holds. Seal new
// data with it. Without a roots event, it is generation 0: the keyed root.
func (sess *Session) DataRoot(ctx context.Context) (devices.Root, error) {
	held, err := sess.DataRoots(ctx)
	if len(held) == 0 {
		return devices.Root{}, err
	}
	if err != nil {
		sess.warnf("arc: the newest data roots did not come: %v\n", err)
	}
	gen := slices.Max(slices.Collect(maps.Keys(held)))
	return devices.Root{Gen: gen, Key: held[gen]}, nil
}

// SealRoot returns the root to seal new self data with: the newest random
// root that this machine keeps on its disk, with no request to the signer
// or the relays. arc sync and arc serve bring the roots up to date. With no
// random root on the disk, it is DataRoot.
func (sess *Session) SealRoot(ctx context.Context) (devices.Root, error) {
	held, err := sess.heldRoots(ctx)
	if err != nil {
		return devices.Root{}, err
	}
	if gen := slices.Max(append(slices.Collect(maps.Keys(held)), 0)); gen > 0 {
		return devices.Root{Gen: gen, Key: held[gen]}, nil
	}
	return sess.DataRoot(ctx)
}

// DataRootAt returns the data root of one generation, to open data that it
// sealed. A root that this machine holds needs no signer and no relay.
func (sess *Session) DataRootAt(ctx context.Context, gen uint32) ([32]byte, error) {
	held, err := sess.heldRoots(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	if key, ok := held[gen]; ok {
		return key, nil
	}
	held, err = sess.DataRoots(ctx)
	if key, ok := held[gen]; ok {
		return key, nil
	}
	if err != nil {
		return [32]byte{}, fmt.Errorf("this machine holds no data root of generation %d, and %w", gen, err)
	}
	return [32]byte{}, fmt.Errorf("this machine holds no data root of generation %d", gen)
}

// DataRoots reads the newest roots event, opens each root that this machine
// may open, and keeps them on disk. A device opens the wrap to its device key,
// and the citizen key opens the wrap to the citizen. When the relays or the
// signer refuse, it returns the roots that this machine already holds, with
// the reason.
func (sess *Session) DataRoots(ctx context.Context) (map[uint32][32]byte, error) {
	held, err := sess.heldRoots(ctx)
	if err != nil {
		return nil, err
	}
	var problems []error
	if _, ok := held[0]; !ok {
		if root, err := sess.KeyedRoot(ctx); err != nil {
			problems = append(problems, err)
		} else {
			held[0] = [32]byte(root)
		}
	}
	newest, unchanged := sess.rootsUnchanged(ctx, held)
	if unchanged {
		// This machine opened the newest roots event already, and holds
		// its roots. It asks the signer nothing.
	} else if r, ok, err := sess.NewestRoots(ctx); err != nil {
		problems = append(problems, err)
	} else if ok {
		var opener nostr.Keyer = sess.Signer
		if sess.Remote {
			opener = sess.Device
		}
		all, err := r.All(ctx, opener, sess.Key.Public)
		if err != nil {
			problems = append(problems, err)
		} else if newest != nil {
			if err := sess.markOpened(newest.ID); err != nil {
				problems = append(problems, err)
			}
		}
		maps.Copy(held, all)
	}
	if err := sess.keepRoots(ctx, held); err != nil {
		problems = append(problems, err)
	}
	return held, errors.Join(problems...)
}

// rootsUnchanged says whether the newest roots event of the store and the
// relays is the one that this machine opened last, and it holds the root of
// its generation. It also returns that newest event.
func (sess *Session) rootsUnchanged(ctx context.Context, held map[uint32][32]byte) (*nostr.Event, bool) {
	filter := devices.RootsFilter(sess.Key.Public)
	sess.Node.Pull(ctx, filter, sess.Relays)
	events, err := sess.Node.Store.Query(filter)
	if err != nil || len(events) == 0 {
		return nil, false
	}
	newest := events[0]
	for _, event := range events[1:] {
		if devices.Newer(event, newest) {
			newest = event
		}
	}
	var opened []byte
	_ = sess.roots.View(func(tx kv.Tx) error {
		if b := tx.Bucket(openedBucket); b != nil {
			opened = slices.Clone(b.Get(openedKey))
		}
		return nil
	})
	return &newest, len(held) > 1 && string(opened) == newest.ID.Hex()
}

func (sess *Session) markOpened(id nostr.ID) error {
	return sess.roots.Update(func(tx kv.Tx) error {
		b, err := tx.CreateBucketIfNotExists(openedBucket)
		if err != nil {
			return err
		}
		return b.Put(openedKey, []byte(id.Hex()))
	})
}

// NewestRoots opens the newest roots event of the store and the relays,
// through the signer. It returns false when no roots event exists.
func (sess *Session) NewestRoots(ctx context.Context) (devices.Roots, bool, error) {
	filter := devices.RootsFilter(sess.Key.Public)
	reports, errs := sess.Node.Pull(ctx, filter, sess.Relays)
	events, err := sess.Node.Store.Query(filter)
	if err != nil {
		return devices.Roots{}, false, err
	}
	unreached := node.Unreached(reports, errs)
	if len(events) == 0 && unreached != nil {
		return devices.Roots{}, false, fmt.Errorf("this machine holds no data roots, and %w", unreached)
	}
	var newest *nostr.Event
	var roots devices.Roots
	var last error
	for _, event := range events {
		if newest != nil && !devices.Newer(event, *newest) {
			continue
		}
		opened, err := devices.OpenRoots(ctx, sess.Signer, sess.Key.Public, event)
		if err != nil {
			last = err
			continue
		}
		newest, roots = &event, opened
	}
	if newest == nil && last != nil {
		return devices.Roots{}, false, last
	}
	return roots, newest != nil, nil
}

// PublishDataRoots makes the roots event follow the device list. The first
// call makes generation 1. When the list drops a device that holds the newest
// root, it makes a new generation for the devices that remain. Otherwise it
// wraps the newest root to each new device. It reports the newest root.
func (sess *Session) PublishDataRoots(ctx context.Context, list devices.List) (devices.Root, error) {
	if err := sess.RequireSigner(); err != nil {
		return devices.Root{}, err
	}
	var keys []nostr.PubKey
	for _, d := range list.Devices {
		keys = append(keys, d.Key)
	}
	r, ok, err := sess.NewestRoots(ctx)
	if err != nil {
		return devices.Root{}, err
	}
	var newest devices.Root
	if !ok {
		r, newest, err = devices.NewGeneration(ctx, sess.Signer, nil, keys, 0)
	} else {
		var all map[uint32][32]byte
		if all, err = r.All(ctx, sess.Signer, sess.Key.Public); err != nil {
			return devices.Root{}, err
		}
		newest = devices.Root{Gen: r.Gen, Key: all[r.Gen]}
		dropped := false
		for d := range r.Devices {
			dropped = dropped || !slices.Contains(keys, d)
		}
		switch {
		case dropped:
			r, newest, err = devices.NewGeneration(ctx, sess.Signer, all, keys, r.At)
		case len(r.Devices) == len(keys):
			return newest, nil
		default:
			r, err = devices.Grant(ctx, sess.Signer, r, newest, keys)
		}
	}
	if err != nil {
		return devices.Root{}, err
	}
	event, err := devices.SealRoots(ctx, sess.Signer, r, nostr.Now())
	if err != nil {
		return devices.Root{}, err
	}
	return newest, sess.publishOwn(ctx, event, "the data roots")
}

// cacheKey seals the roots on the disk of this machine: the device key on a
// device, and the citizen key on a machine that holds it.
func (sess *Session) cacheKey() (keys.Key, error) {
	k := sess.Key
	if sess.Remote {
		k = sess.Device
	}
	if k.Secret == (nostr.SecretKey{}) {
		return keys.Key{}, errors.New("this machine has no device key to seal its data roots: run arc keys device")
	}
	return k, nil
}

// heldRoots reads the roots that this machine keeps on its disk. A root of an
// older arc is 32 bytes in clear text. The next keepRoots seals it.
func (sess *Session) heldRoots(ctx context.Context) (map[uint32][32]byte, error) {
	k, err := sess.cacheKey()
	if err != nil {
		return nil, err
	}
	held := map[uint32][32]byte{}
	err = sess.roots.View(func(tx kv.Tx) error {
		b := tx.Bucket(rootsBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(key, v []byte) error {
			if len(key) != 4 {
				return nil
			}
			gen := binary.BigEndian.Uint32(key)
			if len(v) == 32 {
				held[gen] = [32]byte(v)
				return nil
			}
			text, err := k.Decrypt(ctx, string(v), k.Public)
			if err != nil {
				return fmt.Errorf("the data root of generation %d on this disk does not open: %w", gen, err)
			}
			root, err := hex.DecodeString(text)
			if err != nil || len(root) != 32 {
				return fmt.Errorf("the data root of generation %d on this disk is not 32 bytes", gen)
			}
			held[gen] = [32]byte(root)
			return nil
		})
	})
	return held, err
}

func (sess *Session) keepRoots(ctx context.Context, held map[uint32][32]byte) error {
	k, err := sess.cacheKey()
	if err != nil {
		return err
	}
	sealed := map[uint32]string{}
	for gen, root := range held {
		if sealed[gen], err = k.Encrypt(ctx, hex.EncodeToString(root[:]), k.Public); err != nil {
			return err
		}
	}
	return sess.roots.Update(func(tx kv.Tx) error {
		b, err := tx.CreateBucketIfNotExists(rootsBucket)
		if err != nil {
			return err
		}
		for gen, value := range sealed {
			if err := b.Put(binary.BigEndian.AppendUint32(nil, gen), []byte(value)); err != nil {
				return err
			}
		}
		return nil
	})
}

// PageSigner is the key that signs self data on this machine: the device
// key on a device, and the citizen key on a machine that holds it.
func (sess *Session) PageSigner() nostr.Keyer {
	if sess.Remote {
		return sess.Device
	}
	return sess.Signer
}
