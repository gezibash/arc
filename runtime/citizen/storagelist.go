package citizen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
)

// StorageList returns the newest storage list of this citizen, from the
// store and the relays. It returns false when no list exists. A machine
// that signs through a remote signer opens it there, because the list is
// sealed to the citizen key. It keeps the list on the disk: when no event is
// newer than the list that this machine opened last, it asks the signer
// nothing.
func (sess *Session) StorageList(ctx context.Context) (storage.List, bool, error) {
	filter := storage.ListFilter(sess.Key.Public)
	reports, errs := sess.Node.Pull(ctx, filter, sess.Relays)
	events, err := sess.Node.Store.Query(filter)
	if err != nil {
		return storage.List{}, false, err
	}
	if cached, ok := sess.keptStorageList(ctx); ok && !slices.ContainsFunc(events, func(e nostr.Event) bool { return e.CreatedAt > cached.At }) {
		return cached, true, nil
	}
	if unreached := node.Unreached(reports, errs); len(events) == 0 && unreached != nil {
		return storage.List{}, false, fmt.Errorf("this machine holds no storage list, and %w", unreached)
	}
	var newest *nostr.Event
	var list storage.List
	var last error
	for _, event := range events {
		if newest != nil && !devices.Newer(event, *newest) {
			continue
		}
		opened, err := storage.OpenList(ctx, sess.Signer, sess.Key.Public, event)
		if err != nil {
			last = err
			continue
		}
		newest, list = &event, opened
	}
	if newest == nil && last != nil {
		return storage.List{}, false, last
	}
	if newest != nil {
		if err := sess.keepStorageList(ctx, list); err != nil {
			sess.warnf("arc: the storage list is not kept on this disk: %v\n", err)
		}
	}
	return list, newest != nil, nil
}

// storageListBucket keeps the storage list that this machine last opened,
// sealed with NIP-44 from the cache key to itself, as the device list is. A
// device thus reads the list with no path to the signer, until a newer list
// comes.
var (
	storageListBucket = []byte("storage-list")
	storageListKey    = []byte("list")
)

// keptStorageList reads the storage list on the disk.
func (sess *Session) keptStorageList(ctx context.Context) (storage.List, bool) {
	k, err := sess.cacheKey()
	if err != nil {
		return storage.List{}, false
	}
	var sealed []byte
	_ = sess.roots.View(func(tx kv.Tx) error {
		if b := tx.Bucket(storageListBucket); b != nil {
			sealed = b.Get(storageListKey)
		}
		return nil
	})
	if sealed == nil {
		return storage.List{}, false
	}
	text, err := k.Decrypt(ctx, string(sealed), k.Public)
	if err != nil {
		sess.warnf("arc: the storage list on this disk does not open: %v\n", err)
		return storage.List{}, false
	}
	var list storage.List
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		sess.warnf("arc: the storage list on this disk is not valid: %v\n", err)
		return storage.List{}, false
	}
	return list, true
}

// keepStorageList keeps a storage list that this machine opened on its disk.
func (sess *Session) keepStorageList(ctx context.Context, list storage.List) error {
	k, err := sess.cacheKey()
	if err != nil {
		return err
	}
	text, err := json.Marshal(list)
	if err != nil {
		return err
	}
	sealed, err := k.Encrypt(ctx, string(text), k.Public)
	if err != nil {
		return err
	}
	return sess.roots.Update(func(tx kv.Tx) error {
		b, err := tx.CreateBucketIfNotExists(storageListBucket)
		if err != nil {
			return err
		}
		return b.Put(storageListKey, []byte(sealed))
	})
}

// PublishStorageList seals the list with the citizen key, keeps it, and
// sends it to the relays of the citizen. Only the signer does this. It
// returns the list with the time of its event.
func (sess *Session) PublishStorageList(ctx context.Context, list storage.List) (storage.List, error) {
	if err := sess.RequireSigner(); err != nil {
		return storage.List{}, err
	}
	event, err := storage.SealList(ctx, sess.Signer, list, nostr.Now())
	if err != nil {
		return storage.List{}, err
	}
	list.At = event.CreatedAt
	return list, sess.publishOwn(ctx, event, "the storage list")
}

// RelayURLs returns the URLs of the relays of this citizen.
func (sess *Session) RelayURLs() []string {
	var urls []string
	for _, r := range sess.Relays {
		urls = append(urls, r.Name())
	}
	return urls
}

// FollowStorageList makes the storage installs of this machine follow the
// storage list. It installs the storage app of each entry that is active
// or draining under the name of the entry, and removes the install of each
// entry that is gone. It names each local install that the list does not
// hold. With no list, it changes nothing: an older signer publishes none.
// It returns the list, and false when no list exists.
func (sess *Session) FollowStorageList(ctx context.Context, installs catalog.Installs, out, errs io.Writer) (storage.List, bool, error) {
	list, ok, err := sess.StorageList(ctx)
	if err != nil {
		return storage.List{}, false, fmt.Errorf("the storage list did not come, so this machine syncs with its own installs: %w", SignerOnly(err))
	}
	if !ok {
		return storage.List{}, false, nil
	}
	all, err := installs.List()
	if err != nil {
		return list, true, err
	}
	installed := map[string]catalog.Install{}
	names := map[string]catalog.Install{}
	for _, install := range all {
		names[install.As] = install
		if install.ID == StorageApp {
			installed[install.Provider] = install
		}
	}
	listed := map[string]bool{}
	for _, entry := range list.Entries {
		key := entry.Key.Hex()
		listed[key] = true
		install, have := installed[key]
		switch {
		case entry.State == storage.StateGone && have:
			if _, err := installs.Remove(install.As); err != nil {
				return list, true, err
			}
			fmt.Fprintf(out, "removed %s: the storage list says it is gone\n", install.As)
		case entry.Used() && !have:
			if other, taken := names[entry.Name]; taken {
				fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("%s: the storage list names it, but this machine uses the name for %s of %s; remove that install to sync with it", entry.Name, other.ID, other.Provider)))
				continue
			}
			offer, err := sess.FindOffer(ctx, entry.Key, StorageApp)
			if err != nil {
				fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("%s: the storage list names it, but its storage app was not found: %v", entry.Name, err)))
				continue
			}
			if err := installs.Add(offer, entry.Name); err != nil {
				return list, true, err
			}
			fmt.Fprintf(out, "installed %s from the storage list\n", entry.Name) //arc:raw-ok an entry of the storage list that this citizen signed
		}
	}
	for _, install := range all {
		if install.ID == StorageApp && !listed[install.Provider] {
			fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("%s is not in the storage list; run arc keys storage add on the signer", install.As)))
		}
	}
	return list, true, nil
}
