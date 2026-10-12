package citizen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/iface"
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
	// tick, arc sync with --repair. RepairTURN lets repair use a pipe through
	// TURN.
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

// SyncStorage syncs the self data of this machine with each storage
// citizen that the citizen installed, all at the same time. Each storage
// citizen holds a full copy of each blob. A storage citizen that the
// storage list drains gets no put. A storage citizen that fails does not
// stop the others. With none, the data stays on this machine, and it says
// so. After all of them end, it prints one line for each, in the order of
// the installs.
func (sess *Session) SyncStorage(ctx context.Context, cfg SelfSync) error {
	list, listed, err := sess.FollowStorageList(ctx, cfg.Installs, cfg.Out, cfg.Errs)
	if err != nil {
		fmt.Fprintf(cfg.Errs, "self data: %v\n", err)
	}
	installs, err := storageInstalls(cfg.Installs)
	if err != nil {
		return err
	}
	// A local install that the list does not name syncs as active. A gone
	// entry whose install is still here gets no sync.
	var targets []target
	for _, install := range installs {
		state := storage.StateActive
		if listed {
			if entry, ok := list.Find(install.Provider); ok {
				state = entry.State
			}
		}
		if state != storage.StateGone {
			targets = append(targets, target{install, state})
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(cfg.Out, "self data stays on this machine: no storage citizen; install one with arc install <key> storage")
		return nil
	}
	// A machine that holds the citizen key signs with it. It is the signer,
	// and is on no device list. A device that signs through the signer
	// uses its device key.
	signer := cfg.Device.Secret
	if !sess.Remote {
		signer = sess.Key.Secret
	}
	// The signer sends the grant set first, so that a revocation reaches
	// each storage citizen even when the signer was offline at the revoke.
	if !sess.Remote {
		if list, ok, err := sess.DeviceList(ctx); err != nil {
			fmt.Fprintf(cfg.Errs, "self data: the device list did not come, so no grants went: %v\n", err)
		} else if ok {
			if _, err := sess.PushGrants(ctx, cfg.Installs, list); err != nil {
				fmt.Fprintf(cfg.Errs, "self data: %v\n", err)
			}
		}
	}
	// The keys and the device list come once for all storage citizens: on
	// a phone each one is a question to the signer.
	newest, err := sess.newestSelfKeys(ctx)
	if err != nil {
		fmt.Fprintf(cfg.Errs, "self data: the keys of the blobs did not come, so no storage citizen synced: %v\n", err)
		return err
	}
	syncer := &selfsync.Syncer{
		Citizen: sess.Key.Public,
		Device:  signer,
		Store:   sess.Node.Store,
		Newest:  func(context.Context) (selfsync.Keys, error) { return newest, nil },
		Keys:    once(sess.selfKeysAt),
		Devices: onceList(sess.DeviceList),
		State:   cfg.Dir,
		Repair:  cfg.Repair,
		TURN:    cfg.RepairTURN,
	}
	// Each pipe opens at the same time. Then one sync lists each storage
	// citizen, reads the pieces that this machine lacks, and puts the
	// pieces that the storage citizens lack.
	start := time.Now()
	opened := make([]opening, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() { opened[i] = sess.openOne(ctx, cfg, syncer, t) })
	}
	wg.Wait()
	defer func() {
		for _, o := range opened {
			if o.conn != nil {
				o.conn.Close()
			}
		}
	}()
	sides := make([]selfsync.Side, len(targets))
	for i, t := range targets {
		key, err := nostr.PubKeyFromHex(t.install.Provider)
		if err != nil {
			return fmt.Errorf("%s: %w", t.install.As, err)
		}
		sides[i] = selfsync.Side{Key: key, Label: t.install.As, Active: t.state == storage.StateActive}
		if opened[i].conn != nil {
			sides[i].Conn = opened[i].conn
			sides[i].Relayed = opened[i].conn.Path == "relay"
		}
	}
	report, err := syncer.SyncAll(ctx, sides)

	var failed []error
	if err != nil {
		failed = append(failed, err)
		fmt.Fprintf(cfg.Errs, "self data: %v\n", err)
	}
	for i, o := range opened {
		as := targets[i].install.As
		_, _ = cfg.Errs.Write(o.notes.Bytes())
		side := report.Sides[i]
		if o.err == nil && side.Err != nil {
			o.err = side.Err
		}
		if o.err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", as, o.err))
			fmt.Fprintf(cfg.Errs, "%s: self data: %v\n", as, o.err)
			if o.silent >= selfsync.SilentWarning {
				fmt.Fprintf(cfg.Errs, "%s: no answer for %d days; if it is gone, run arc keys storage rm %s --gone on the signer\n", as, int(o.silent/(24*time.Hour)), as)
			}
			continue
		}
		took := o.opened.Sub(start)
		if side.Done.After(o.opened) {
			took = side.Done.Sub(start)
		}
		fmt.Fprintf(cfg.Out, "%s: self data received %d, sent %d, already held %d in %s", as, side.Got, side.Stored, side.Held, took.Round(10*time.Millisecond))
		if targets[i].state == storage.StateDraining {
			fmt.Fprint(cfg.Out, "; draining, so nothing new goes to it")
		}
		fmt.Fprintln(cfg.Out)
		if side.Old {
			fmt.Fprintf(cfg.Errs, "%s runs an older storage app: it holds full copies; update it for erasure coding\n", as)
		}
		if side.Audited > 0 {
			fmt.Fprintf(cfg.Out, "%s: audit checked %d pieces, %d failed\n", as, side.Audited, side.AuditFailed)
		}
		if side.Suspect {
			fmt.Fprintf(cfg.Errs, "%s is suspect: %d pieces failed the audits of today; repair replaces them; if it is gone, run arc keys storage rm %s --gone on the signer\n", as, side.FailedToday, as)
		}
		if side.Empty {
			fmt.Fprintf(cfg.Out, "%s holds no piece now; run arc keys storage rm %s --gone on the signer\n", as, as)
		}
	}
	fmt.Fprintf(cfg.Out, "self data: took %d records, already held %d", report.Received, report.Held)
	if report.T > 0 {
		fmt.Fprintf(cfg.Out, ", coded %d new records into pieces, %d of %d rebuild each", report.Sent, report.K, report.T)
	}
	fmt.Fprintln(cfg.Out)
	for _, reason := range report.Refused {
		fmt.Fprintf(cfg.Errs, "  refused %s\n", reason)
	}
	if report.Waiting > 0 {
		failed = append(failed, fmt.Errorf("%d records wait", report.Waiting))
		fmt.Fprintf(cfg.Errs, "self data: %d records wait: a new blob needs %d storage citizens that answer; the next sync puts them\n", report.Waiting, report.K)
	}
	rep := report.Repair
	if cfg.Repair {
		fmt.Fprintf(cfg.Out, "self data: repair rebuilt %d pieces, moved %d, sealed %d records again, coded %d blobs of format 1 into pieces, removed %d blobs of replaced records, deleted %d; %d bytes down, %d bytes up\n",
			rep.Rebuilt, rep.Moved, rep.Sealed, rep.Coded, rep.Replaced, rep.Deleted, rep.Down, rep.Up)
	} else {
		if rep.Pending > 0 {
			fmt.Fprintf(cfg.Errs, "self data: %d blobs need repair; arc serve repairs them on its tick, or run arc sync --repair\n", rep.Pending)
		}
		if rep.Replaced > 0 {
			fmt.Fprintf(cfg.Out, "self data: %d blobs of replaced or deleted records wait; arc serve deletes them on its tick, or run arc sync --repair\n", rep.Replaced)
		}
	}
	if rep.Old > 0 {
		fmt.Fprintf(cfg.Out, "self data: %d blobs of format 1 remain; repair codes them into pieces\n", rep.Old)
	}
	if rep.WaitDirect > 0 && cfg.Repair {
		var relayed []string
		for i, o := range opened {
			if o.conn != nil && o.conn.Path == "relay" {
				relayed = append(relayed, targets[i].install.As)
			}
		}
		fmt.Fprintf(cfg.Errs, "self data: the repair of %d blobs waits for a direct path: the pipe to %s goes through TURN; arc sync --repair --turn allows TURN\n",
			rep.WaitDirect, strings.Join(relayed, ", "))
	}
	if report.Short > 0 {
		fmt.Fprintf(cfg.Out, "self data: %d blobs have fewer than k pieces of one seal on the storage citizens: a deletion that did not end, or lost pieces; repair ends them\n", report.Short)
	}
	if n := len(report.Unreadable); n > 0 {
		failed = append(failed, fmt.Errorf("%d blobs cannot be read now", n))
		fmt.Fprintf(cfg.Errs, "self data: %d blobs cannot be read now: fewer of their pieces answered than rebuild them; start the storage citizens that are stopped, and sync again\n", n)
		for _, reason := range report.Unreadable {
			fmt.Fprintf(cfg.Errs, "  unreadable %s\n", reason)
		}
	}
	if report.Kept != nil {
		sess.moveDrafts(ctx, cfg, report.Kept)
	}
	return errors.Join(failed...)
}

// target is one storage citizen of a sync, with its state in the storage
// list.
type target struct {
	install catalog.Install
	state   string
}

// opening is the pipe to one storage citizen. notes holds what the pipe
// printed, so that the notes of two storage citizens do not mix.
type opening struct {
	conn   *storage.Conn
	opened time.Time
	err    error
	silent time.Duration
	notes  bytes.Buffer
}

// openOne opens the pipe to one storage citizen, and keeps the record of
// its silence.
func (sess *Session) openOne(ctx context.Context, cfg SelfSync, syncer *selfsync.Syncer, t target) (o opening) {
	install := t.install
	client := storage.Client{
		Call: func(ctx context.Context, session string) ([]byte, error) {
			return sess.openStorage(ctx, cfg.Installs, install.As, session)
		},
		TURN: cfg.TURN, Options: cfg.Options, Notes: &o.notes,
		NoRelay: cfg.Path == direct.DirectOnly, OnlyRelay: cfg.Path == direct.RelayOnly,
	}
	conn, err := client.Open(ctx, 0)
	o.opened = time.Now()
	if err != nil {
		o.err = err
		if silent, serr := syncer.Unanswered(install.Provider, time.Now()); serr != nil {
			o.err = errors.Join(err, serr)
		} else {
			o.silent = silent
		}
		return o
	}
	if err := syncer.Answered(install.Provider, time.Now()); err != nil {
		conn.Close()
		o.err = err
		return o
	}
	o.conn = conn
	return o
}

// once gives the keys of each generation from get, and asks get once for
// each generation that it gave.
func once(get func(context.Context, uint32) (selfsync.Keys, error)) func(context.Context, uint32) (selfsync.Keys, error) {
	var mu sync.Mutex
	held := map[uint32]selfsync.Keys{}
	return func(ctx context.Context, gen uint32) (selfsync.Keys, error) {
		mu.Lock()
		defer mu.Unlock()
		if k, ok := held[gen]; ok {
			return k, nil
		}
		k, err := get(ctx, gen)
		if err == nil {
			held[gen] = k
		}
		return k, err
	}
}

// onceList gives the device list from get, and asks get until it answers.
func onceList(get func(context.Context) (devices.List, bool, error)) func(context.Context) (devices.List, bool, error) {
	var mu sync.Mutex
	var done bool
	var list devices.List
	var ok bool
	return func(ctx context.Context) (devices.List, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return list, ok, nil
		}
		l, has, err := get(ctx)
		if err != nil {
			return l, has, err
		}
		done, list, ok = true, l, has
		return list, ok, nil
	}
}

// openStorage runs the manifest command "open" of an installed storage
// app, as "arc <name> open <session>" does. The command adds the pseudonym
// of the account, and makes the ARC call.
func (sess *Session) openStorage(ctx context.Context, installs catalog.Installs, name, session string) ([]byte, error) {
	in, err := sess.Installed(ctx, installs, name)
	if err != nil {
		return nil, err
	}
	env := &Environment{Session: sess, Installs: installs, Tool: name}
	var out, errs bytes.Buffer
	err = iface.Run(ctx, env, in, []string{"open", session}, iface.Stdio{In: strings.NewReader(""), Out: &out, Err: &errs})
	if err != nil {
		text := strings.TrimSpace(errs.String())
		refusal := text + " " + err.Error()
		switch {
		case strings.Contains(refusal, "not_owner"):
			return nil, errors.New("the storage citizen refused this machine: its key is neither an account key in STORAGE_OWNERS nor a device key with a grant; the signer sends grants with arc keys devices add and arc sync")
		case strings.Contains(refusal, "busy"):
			return nil, errors.New("the storage citizen runs too many sessions now; try again later")
		case text != "":
			return nil, fmt.Errorf("the ARC call failed: %s", text)
		}
		return nil, fmt.Errorf("the ARC call failed: %w", err)
	}
	return bytes.TrimSpace(out.Bytes()), nil
}
