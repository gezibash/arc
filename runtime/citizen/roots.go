package citizen

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/node"
)

// rootsBucket keeps each data root that this machine holds, by generation.
var rootsBucket = []byte("data-roots")

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

// DataRootAt returns the data root of one generation, to open data that it
// sealed.
func (sess *Session) DataRootAt(ctx context.Context, gen uint32) ([32]byte, error) {
	held, err := sess.DataRoots(ctx)
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
	held, err := sess.heldRoots()
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
	if r, ok, err := sess.NewestRoots(ctx); err != nil {
		problems = append(problems, err)
	} else if ok {
		var opener nostr.Keyer = sess.Signer
		if sess.Remote {
			opener = sess.Device
		}
		all, err := r.All(ctx, opener, sess.Key.Public)
		if err != nil {
			problems = append(problems, err)
		}
		maps.Copy(held, all)
	}
	if err := sess.keepRoots(held); err != nil {
		problems = append(problems, err)
	}
	return held, errors.Join(problems...)
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

func (sess *Session) heldRoots() (map[uint32][32]byte, error) {
	held := map[uint32][32]byte{}
	err := sess.roots.View(func(tx kv.Tx) error {
		b := tx.Bucket(rootsBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if len(k) == 4 && len(v) == 32 {
				held[binary.BigEndian.Uint32(k)] = [32]byte(v)
			}
			return nil
		})
	})
	return held, err
}

func (sess *Session) keepRoots(held map[uint32][32]byte) error {
	return sess.roots.Update(func(tx kv.Tx) error {
		b, err := tx.CreateBucketIfNotExists(rootsBucket)
		if err != nil {
			return err
		}
		for gen, key := range held {
			if err := b.Put(binary.BigEndian.AppendUint32(nil, gen), key[:]); err != nil {
				return err
			}
		}
		return nil
	})
}
