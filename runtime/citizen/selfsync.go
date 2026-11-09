package citizen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/selfsync"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/storage"
)

// StorageApp is the manifest ID of the storage app. Each install of it is
// a storage citizen of this citizen.
const StorageApp = "storage"

// SelfSync is what a sync of self data needs from the entry point.
type SelfSync struct {
	Installs catalog.Installs
	// Device is the device key of this machine. It signs each change on a
	// machine that signs through a remote signer.
	Device keys.Key
	// Dir is the directory of the sync state of this citizen on this
	// machine.
	Dir string
	// TURN gets a credential for the TURN server of the first relay.
	TURN    func(ctx context.Context) (direct.Credential, error)
	Options direct.Options
	// Path is the path of the citizen (arc path): direct uses no TURN, and
	// turn uses only TURN. auto and session try a direct path first.
	Path direct.Policy
	// Out gets one line for each storage citizen. Errs gets the notes of
	// the pipe and each refused blob.
	Out, Errs io.Writer
	// Repair lets the sync audit and repair pieces: arc serve on each
	// tick, arc sync with --repair. RepairTURN lets a rebuild and an audit
	// use a pipe through TURN; the path turn does too. A deletion always may.
	Repair, RepairTURN bool
}

// selfKeys is the one place that chooses the keys of the blobs. A new blob
// is sealed with the newest data root that this machine holds. A blob of
// generation g opens with the data root of g. A device that the signer
// revoked gets no newer data root, so it cannot open a blob that another
// device seals after the revocation. The ID key stays on the keyed root.
func (sess *Session) selfKeys(ctx context.Context, root devices.Root) (selfsync.Keys, error) {
	keyed, err := sess.KeyedRoot(ctx)
	if err != nil {
		return selfsync.Keys{}, err
	}
	return selfsync.NewKeys(root.Gen, root.Key[:], keyed)
}

// newestSelfKeys returns the keys of the newest generation of this machine.
func (sess *Session) newestSelfKeys(ctx context.Context) (selfsync.Keys, error) {
	root, err := sess.DataRoot(ctx)
	if err != nil {
		return selfsync.Keys{}, err
	}
	return sess.selfKeys(ctx, root)
}

// selfKeysAt returns the keys of one generation.
func (sess *Session) selfKeysAt(ctx context.Context, gen uint32) (selfsync.Keys, error) {
	key, err := sess.DataRootAt(ctx, gen)
	if err != nil {
		return selfsync.Keys{}, fmt.Errorf("the blob is of generation %d: %w", gen, err)
	}
	return sess.selfKeys(ctx, devices.Root{Gen: gen, Key: key})
}

// StorageSigner is the key that signs the calls to one storage citizen. A
// machine with the citizen key signs with the account key for that storage
// citizen. A device that signs through a remote signer signs with its device
// key, which a grant set of the account key names. The storage citizen
// never sees the citizen key.
func (sess *Session) StorageSigner(storage nostr.PubKey) (keys.Signer, error) {
	if !sess.Remote {
		return selfsync.AccountKey(sess.Key.Secret, storage)
	}
	if sess.Device.Public == (nostr.PubKey{}) {
		return nil, errors.New("this machine has no device key, so it cannot call a storage citizen: run arc keys device")
	}
	return sess.Device, nil
}

// AccountKey is the account key of this citizen for one storage citizen.
// Only a machine with the citizen key derives it.
func (sess *Session) AccountKey(storage nostr.PubKey) (keys.Key, error) {
	if err := sess.RequireSigner(); err != nil {
		return keys.Key{}, err
	}
	return selfsync.AccountKey(sess.Key.Secret, storage)
}

// PushGrants sends the grant set of the device list to each storage citizen
// that this citizen installed, and to each storage citizen that the storage
// list names as active or draining. The account key of each storage citizen
// signs the call. A device key that the list does not hold loses its grant.
// It returns the storage citizens that took the set, and one error for each
// storage citizen that did not.
func (sess *Session) PushGrants(ctx context.Context, installs catalog.Installs, list devices.List) ([]catalog.Install, error) {
	if err := sess.RequireSigner(); err != nil {
		return nil, err
	}
	targets, err := storageInstalls(installs)
	if err != nil {
		return nil, err
	}
	// The storage list only adds targets. If it does not come, the set
	// still goes to each install; a sync names the missing list.
	if storageList, ok, err := sess.StorageList(ctx); err == nil && ok {
		targets = withListed(targets, storageList)
	}
	body, err := grantsBody(list)
	if err != nil {
		return nil, err
	}
	var reached []catalog.Install
	var failed []error
	for _, install := range targets {
		if err := sess.pushGrants(ctx, install, body); err != nil {
			failed = append(failed, fmt.Errorf("%s: the grants did not reach the storage citizen: %w", install.As, err))
			continue
		}
		reached = append(reached, install)
	}
	return reached, errors.Join(failed...)
}

func grantsBody(list devices.List) ([]byte, error) {
	grants := storage.Grants{Version: 1, Devices: []string{}, At: int64(list.At)}
	for _, d := range list.Devices {
		grants.Devices = append(grants.Devices, d.Key.Hex())
	}
	return json.Marshal(grants)
}

func (sess *Session) pushGrants(ctx context.Context, install catalog.Install, body []byte) error {
	provider, err := nostr.PubKeyFromHex(install.Provider)
	if err != nil {
		return err
	}
	account, err := selfsync.AccountKey(sess.Key.Secret, provider)
	if err != nil {
		return err
	}
	request := call.Request{Capability: install.ID, Method: "RAW", Path: "/", Body: string(body)}
	reply, _, _, err := sess.LiveCallAs(ctx, account, provider, request, call.Timeout)
	if err != nil {
		return err
	}
	if reply.Err != "" {
		return errors.New(reply.Err)
	}
	return nil
}

// withListed adds each storage citizen of the storage list that is active
// or draining, and that no install names.
func withListed(targets []catalog.Install, list storage.List) []catalog.Install {
	for _, entry := range list.Entries {
		key := entry.Key.Hex()
		if entry.Used() && !slices.ContainsFunc(targets, func(i catalog.Install) bool { return i.Provider == key }) {
			targets = append(targets, catalog.Install{Provider: key, ID: StorageApp, As: entry.Name})
		}
	}
	return targets
}

// storageInstalls returns each storage citizen that the citizen installed.
func storageInstalls(installs catalog.Installs) ([]catalog.Install, error) {
	all, err := installs.List()
	if err != nil {
		return nil, err
	}
	var out []catalog.Install
	for _, install := range all {
		if install.ID == StorageApp && install.As != "" {
			out = append(out, install)
		}
	}
	return out, nil
}

// importedFile marks that this machine imported the drafts of the relays.
const importedFile = "imported"

// ImportDrafts gets the self data of this citizen from each relay, and
// keeps it in the local store, once. A second run gets nothing new: the
// store keeps each event once. With again, it runs even if it ran before,
// and the drafts that are left on the relays are deleted again. It never
// keeps a deletion that a device sent for the drafts that it moved to a
// storage citizen: in the store that deletion deletes the drafts.
func (sess *Session) ImportDrafts(ctx context.Context, dir string, again bool) (int, error) {
	marker := filepath.Join(dir, importedFile)
	if _, err := os.Stat(marker); err == nil && !again {
		return 0, nil
	}
	if len(sess.Relays) == 0 {
		return 0, nil
	}
	if again {
		if err := os.Remove(filepath.Join(dir, movedFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	received, answered := 0, 0
	var errs []error
	for _, t := range sess.Relays {
		batch, err := t.Fetch(ctx, selfsync.Filter(sess.Key.Public))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		answered++
		for _, event := range batch.Events {
			if draft.IsMoved(event) {
				continue
			}
			result, err := sess.Node.Store.Save(event)
			if err != nil {
				return received, err
			}
			if result.Outcome == store.Stored {
				received++
			}
		}
	}
	if answered == 0 && len(errs) > 0 {
		return received, fmt.Errorf("the import of the drafts on the relays is not complete: no relay answered: %w", errors.Join(errs...))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return received, err
	}
	return received, os.WriteFile(marker, []byte(nostr.Now().Time().UTC().Format("2006-01-02T15:04:05Z")+"\n"), 0o600)
}
