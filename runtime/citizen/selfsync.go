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
	"github.com/gezibash/arc/internal/writelock"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/runtime/selfsync"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
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
		fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: %v", err)))
	}
	if listed && !sess.Remote {
		list = sess.pruneStorageList(ctx, list, cfg)
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
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: the device list did not come, so no grants went: %v", err)))
		} else if ok {
			if _, err := sess.PushGrants(ctx, cfg.Installs, list); err != nil {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: %v", err)))
			}
		}
	}
	// The keys and the device list come once for all storage citizens: on
	// a phone each one is a question to the signer.
	newest, err := sess.newestSelfKeys(ctx)
	if err != nil {
		fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: the keys of the blobs did not come, so no storage citizen synced: %v", err)))
		return err
	}
	syncer := &selfsync.Syncer{
		Citizen: sess.Key.Public,
		Device:  signer,
		Store:   sess.store,
		Bodies:  sess.bodies,
		Newest:  func(context.Context) (selfsync.Keys, error) { return newest, nil },
		Keys:    once(sess.selfKeysAt),
		Devices: onceList(sess.DeviceList),
		Opener:  sess.pageOpener(cfg.Installs),
		Writing: func() (bool, error) { return writelock.Busy(sess.storeDir) },
		State:   cfg.Dir,
		Repair:  cfg.Repair,
		// On the path turn, each pipe goes through TURN, so a rebuild may too.
		TURN: cfg.RepairTURN || cfg.Path == direct.RelayOnly,
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
		fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: %v", err)))
	}
	var empty []int
	usages := make([]*storage.Usage, len(opened))
	for i, o := range opened {
		as := targets[i].install.As
		_, _ = cfg.Out.Write(o.said.Bytes())
		_, _ = cfg.Errs.Write(o.notes.Bytes())
		side := report.Sides[i]
		if o.err == nil && side.Err != nil {
			o.err = side.Err
		}
		if o.err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", as, o.err))
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: self data: %v", as, o.err)))
			if o.silent >= selfsync.SilentWarning {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: no answer for %d days; if it is gone, run arc keys storage rm %s --gone on the signer", as, int(o.silent/(24*time.Hour)), as)))
			}
			continue
		}
		took := o.opened.Sub(start)
		if side.Done.After(o.opened) {
			took = side.Done.Sub(start)
		}
		fmt.Fprintf(cfg.Out, "%s: self data received %d, sent %d, already held %d in %s", as, side.Got, side.Stored, side.Held, took.Round(10*time.Millisecond))
		if o.conn != nil {
			fmt.Fprintf(cfg.Out, "; path %s", o.conn.Path)
		}
		if targets[i].state == storage.StateDraining {
			fmt.Fprint(cfg.Out, "; draining, so nothing new goes to it")
		}
		fmt.Fprintln(cfg.Out)
		if side.Old {
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s runs an older storage app: it holds full copies; update it for erasure coding", as)))
		}
		if side.Audited > 0 {
			fmt.Fprintf(cfg.Out, "%s: audit checked %d pieces, %d failed\n", as, side.Audited, side.AuditFailed)
		}
		if side.Suspect {
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s is suspect: %d pieces failed the audits of today; repair replaces them; if it is gone, run arc keys storage rm %s --gone on the signer", as, side.FailedToday, as)))
		}
		if o.conn != nil {
			if u, err := o.conn.Usage(ctx); err != nil {
				fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the usage did not come: %v", as, err)))
			} else {
				usages[i] = &u
				fmt.Fprintf(cfg.Out, "%s: %s\n", as, usageLine(u))
			}
		}
		switch {
		case side.Empty && o.locked && !sess.Remote && listed:
			empty = append(empty, i)
		case side.Empty && o.locked:
			fmt.Fprintf(cfg.Out, "%s holds no piece now; the signer marks it gone at its next sync\n", as)
		case side.Empty:
			fmt.Fprintf(cfg.Out, "%s holds no piece now; run arc keys storage rm %s --gone on the signer\n", as, as)
		}
	}
	if len(empty) > 0 {
		if err := sess.markGone(ctx, cfg, list, targets, opened, usages, empty); err != nil {
			failed = append(failed, err)
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: %v", err)))
		}
	}
	fmt.Fprintf(cfg.Out, "self data: took %d records, already held %d", report.Received, report.Held)
	if report.T > 0 {
		fmt.Fprintf(cfg.Out, ", coded %d new records into pieces, %d of %d rebuild each", report.Sent, report.K, report.T)
	}
	fmt.Fprintln(cfg.Out)
	for _, reason := range report.Refused {
		fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("  refused %s", reason)))
	}
	if n, err := sess.PruneBodies(); err != nil {
		fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: the bodies of deleted drafts stay: %v", err)))
	} else if n > 0 {
		fmt.Fprintf(cfg.Out, "self data: removed %d bodies of deleted or replaced drafts\n", n)
	}
	if report.Waiting > 0 {
		failed = append(failed, fmt.Errorf("%d records wait", report.Waiting))
		fmt.Fprintf(cfg.Errs, "self data: %d records wait: a new blob needs %d storage citizens that answer; the next sync puts them\n", report.Waiting, report.K)
	}
	rep := report.Repair
	if cfg.Repair {
		// The blobs of a deletion of parts that no page names get the words
		// of journal delete, not those of replaced records.
		unnamed := ""
		if rep.Unnamed > 0 {
			unnamed = fmt.Sprintf("removed %d blobs of parts and checkpoints that no page names, ", rep.Unnamed)
		}
		fmt.Fprintf(cfg.Out, "self data: repair rebuilt %d pieces, moved %d, sealed %d records again, coded %d blobs of format 1 into pieces, %sremoved %d blobs of replaced records, deleted %d; %d bytes down, %d bytes up\n",
			rep.Rebuilt, rep.Moved, rep.Sealed, rep.Coded, unnamed, rep.Replaced, rep.Deleted, rep.Down, rep.Up)
		if rep.UnnamedParts+rep.UnnamedCheckpoints > 0 {
			count := func(n int, one string) string {
				if n == 1 {
					return "1 " + one
				}
				return fmt.Sprintf("%d %ss", n, one)
			}
			fmt.Fprintf(cfg.Out, "self data: repair removed %s and %s that no page names, as a write that failed leaves them; the next arc sync --repair frees them on your storage citizens\n",
				count(rep.UnnamedParts, "part"), count(rep.UnnamedCheckpoints, "checkpoint"))
		}
	} else {
		if rep.Pending > 0 {
			fmt.Fprintf(cfg.Errs, "self data: %d blobs need repair; arc serve repairs them on its tick, or run arc sync --repair\n", rep.Pending)
		}
		if rep.Replaced > 0 {
			fmt.Fprintf(cfg.Out, "self data: %d blobs of replaced or deleted records wait; arc serve deletes them on its tick, or run arc sync --repair\n", rep.Replaced)
		}
		if rep.Unnamed > 0 {
			fmt.Fprintf(cfg.Out, "self data: %d blobs of parts and checkpoints that no page names wait; arc serve frees them on its tick, or run arc sync --repair\n", rep.Unnamed)
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
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("  unreadable %s", reason)))
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
	// locked says that the account of a draining storage citizen takes no
	// put now, from any device.
	locked bool
	said   bytes.Buffer
	notes  bytes.Buffer
}

// openOne opens the pipe to one storage citizen, and keeps the record of
// its silence.
func (sess *Session) openOne(ctx context.Context, cfg SelfSync, syncer *selfsync.Syncer, t target) (o opening) {
	install := t.install
	// A repair opens its pipes as a sync does: it deletes over TURN, and
	// waits for a direct pipe only to rebuild or audit (docs/storage/SPEC.md,
	// section 4.6). So it never adds a direct failure inside the window.
	conn, err := openPipe(syncer, install.Provider, install.As, cfg.Path, time.Now, &o.notes,
		func(noRelay, onlyRelay bool) (*storage.Conn, error) {
			client := storage.Client{
				Call: func(ctx context.Context, session string) ([]byte, error) {
					reply, err := sess.openStorage(ctx, cfg.Installs, install.As, session)
					if err != nil {
						return nil, callError{err}
					}
					return reply, nil
				},
				TURN: cfg.TURN, Options: cfg.Options, Notes: &o.notes, NoRelay: noRelay, OnlyRelay: onlyRelay,
			}
			return client.Open(ctx, 0)
		})
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
	o.locked, o.err = sess.setDrain(ctx, conn, t, &o.said, &o.notes)
	return o
}

// callError is an error of the ARC call that carries the offer of a pipe:
// the storage citizen did not answer, so another attempt does not help.
type callError struct{ error }

func (e callError) Unwrap() error { return e.error }

// openPipe opens the pipe to one storage citizen with the path of the
// citizen. With the path auto or session, it records whether the direct
// attempts found a path. For selfsync.TURNFirstFor after they found none,
// a sync tries the TURN server of the relay first, and the direct attempts
// only if that fails (docs/storage/SPEC.md, section 4.6). open runs the
// attempts.
func openPipe(syncer *selfsync.Syncer, name, as string, policy direct.Policy, now func() time.Time, notes io.Writer,
	open func(noRelay, onlyRelay bool) (*storage.Conn, error)) (*storage.Conn, error) {
	if policy == direct.DirectOnly || policy == direct.RelayOnly {
		return open(policy == direct.DirectOnly, policy == direct.RelayOnly)
	}
	record := func(conn *storage.Conn, found bool) (*storage.Conn, error) {
		if err := syncer.DirectPath(name, now(), found); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	until, err := syncer.TURNFirst(name, now())
	if err != nil {
		return nil, err
	}
	if !until.IsZero() {
		fmt.Fprintln(notes, terminal.EscapeLine(fmt.Sprintf("%s: the direct attempts found no path; until %s each sync tries the TURN server of the relay first, so the bytes go through the relay",
			as, until.Local().Format("15:04"))))
		conn, err := open(false, true)
		if err == nil || errors.As(err, new(callError)) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return conn, err
		}
		fmt.Fprintln(notes, terminal.EscapeLine(fmt.Sprintf("%s: no path through the relay: %v; trying the direct attempts", as, err)))
		if conn, err = open(true, false); err != nil {
			return nil, err
		}
		return record(conn, true)
	}

	conn, err := open(false, false)
	if err != nil {
		return nil, err
	}
	return record(conn, conn.Path != "relay")
}

// setDrain makes the account follow the storage list before the sync
// lists it. Each device sends a drain request to a draining storage
// citizen, so that it refuses each put, also from an older device. Only
// the signer ends a drain, for an entry that the list names active: a
// device can hold an older list. It reports if the account takes no put.
func (sess *Session) setDrain(ctx context.Context, conn *storage.Conn, t target, out, notes io.Writer) (bool, error) {
	as := t.install.As
	switch {
	case t.state == storage.StateDraining:
		usage, err := conn.Drain(ctx, false)
		if errors.Is(err, storage.ErrOlderApp) {
			fmt.Fprintf(notes, "%s runs a storage app from before drain requests: it still takes a put from an older device; update it\n", as)
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("the drain request failed: %w", err)
		}
		return usage.State == storage.StateDraining || usage.State == storage.StateClosing || usage.State == storage.StateClosed, nil
	case t.state == storage.StateActive && !sess.Remote:
		usage, err := conn.Usage(ctx)
		if err != nil || usage.State != storage.StateDraining {
			return false, err
		}
		if _, err := conn.Drain(ctx, true); err != nil {
			fmt.Fprintf(notes, "%s: the storage list names it active, but the end of its drain failed, so it takes no put: %s\n", as, terminal.EscapeLine(err.Error()))
			return false, nil
		}
		fmt.Fprintf(out, "%s: the storage list names it active, so its drain ended\n", as)
	}
	return false, nil
}

// usageLine says how much of its quota an account uses: the numbers of
// arc-storage usage, in units for a person.
func usageLine(u storage.Usage) string {
	line := fmt.Sprintf("%s of %s in use, %d blobs", storage.HumanSize(u.Used), storage.HumanSize(u.Quota), u.Blobs)
	if u.Plan != "" {
		line += fmt.Sprintf(", plan %s, %s", u.Plan, u.State)
	}
	if u.Expires > 0 {
		line += ", ends " + time.Unix(u.Expires, 0).UTC().Format(time.DateOnly)
	}
	return line
}

// markGone sets gone in the storage list for each draining storage
// citizen that holds no piece and no blob, and takes no put. Only the
// signer does this. The usage of the account confirms that it is empty.
// The signer then closes the account on the storage citizen, which
// removes it.
func (sess *Session) markGone(ctx context.Context, cfg SelfSync, list storage.List, targets []target, opened []opening, usages []*storage.Usage, empty []int) error {
	var gone []int
	for _, i := range empty {
		as := targets[i].install.As
		usage := usages[i]
		if usage == nil {
			return fmt.Errorf("%s: the usage did not come, so it stays draining", as)
		}
		if usage.Blobs != 0 {
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the listing is empty, but its usage counts %d blobs, so it stays draining", as, usage.Blobs)))
			continue
		}
		entry, ok := list.Find(targets[i].install.Provider)
		if !ok {
			continue
		}
		entry.State, entry.Since = storage.StateGone, nostr.Now()
		var err error
		if list, err = list.Put(entry); err != nil {
			return err
		}
		fmt.Fprintf(cfg.Out, "%s holds no piece now and takes no put, so the storage list marks it gone\n", as)
		gone = append(gone, i)
	}
	if len(gone) == 0 {
		return nil
	}
	names := make([]string, len(gone))
	for n, i := range gone {
		names[n] = targets[i].install.As
	}
	if _, err := sess.PublishStorageList(ctx, list); err != nil {
		return fmt.Errorf("the storage list did not change, so %s stays draining: %w", strings.Join(names, ", "), err)
	}
	for _, i := range gone {
		as := targets[i].install.As
		fmt.Fprintf(cfg.Out, "storage list: %s is gone; each device removes its install at its next sync\n", as)
		// The pipe ends first: the close ends each pipe of the account.
		opened[i].conn.Close()
		opened[i].conn = nil
		provider, err := nostr.PubKeyFromHex(targets[i].install.Provider)
		if err != nil {
			return err
		}
		reply, err := sess.CloseStorage(ctx, provider)
		switch {
		case err != nil:
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the account stays, empty and draining: %v", as, err)))
		case !reply.Closed:
			fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("%s: the account stays: %s", as, reply.Message)))
		default:
			fmt.Fprintf(cfg.Out, "%s: the account is closed, and the storage citizen removed it; arc keys storage add asks for a new one\n", as)
		}
	}
	return nil
}

// pruneStorageList removes each entry that is gone for 30 days or more,
// and publishes the list. Only the signer does this. If the list does not
// publish, it returns the list as it was.
func (sess *Session) pruneStorageList(ctx context.Context, list storage.List, cfg SelfSync) storage.List {
	pruned, removed := list.Prune(time.Now())
	if len(removed) == 0 {
		return list
	}
	if _, err := sess.PublishStorageList(ctx, pruned); err != nil {
		fmt.Fprintln(cfg.Errs, terminal.EscapeLine(fmt.Sprintf("self data: the storage list keeps its old gone entries: %v", err)))
		return list
	}
	for _, e := range removed {
		fmt.Fprintf(cfg.Out, "storage list: removed %s, gone since %s\n", e.Name, e.Since.Time().UTC().Format("2006-01-02")) //arc:raw-ok an entry of the storage list that this citizen signed
	}
	return pruned
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
