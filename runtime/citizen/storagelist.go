package citizen

import (
	"context"
	"fmt"
	"io"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/sdk/storage"
)

// StorageList returns the newest storage list of this citizen, from the
// store and the relays. It returns false when no list exists. A machine
// that signs through a remote signer opens it there, because the list is
// sealed to the citizen key.
func (sess *Session) StorageList(ctx context.Context) (storage.List, bool, error) {
	filter := storage.ListFilter(sess.Key.Public)
	reports, errs := sess.Node.Pull(ctx, filter, sess.Relays)
	events, err := sess.Node.Store.Query(filter)
	if err != nil {
		return storage.List{}, false, err
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
	return list, newest != nil, nil
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
		return storage.List{}, false, fmt.Errorf("the storage list did not come, so this machine syncs with its own installs: %w", err)
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
				fmt.Fprintf(errs, "%s: the storage list names it, but this machine uses the name for %s of %s; remove that install to sync with it\n", entry.Name, other.ID, other.Provider)
				continue
			}
			offer, err := sess.FindOffer(ctx, entry.Key, StorageApp)
			if err != nil {
				fmt.Fprintf(errs, "%s: the storage list names it, but its storage app was not found: %v\n", entry.Name, err)
				continue
			}
			if err := installs.Add(offer, entry.Name); err != nil {
				return list, true, err
			}
			fmt.Fprintf(out, "installed %s from the storage list\n", entry.Name)
		}
	}
	for _, install := range all {
		if install.ID == StorageApp && !listed[install.Provider] {
			fmt.Fprintf(errs, "%s is not in the storage list; run arc keys storage add on the signer\n", install.As)
		}
	}
	return list, true, nil
}
