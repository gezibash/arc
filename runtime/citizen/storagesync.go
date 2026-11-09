package citizen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/internal/writelock"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/selfsync"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
)

// SyncStorage syncs the self data of this machine with each storage
// citizen that the citizen installed, all at the same time. Each storage
// citizen holds a full copy of each blob. A storage citizen that the
// storage list drains gets no put. A storage citizen that fails does not
// stop the others. With none, the data stays on this machine, and it says
// so. After all of them end, it prints one line for each, in the order of
// the installs.
func (sess *Session) SyncStorage(ctx context.Context, cfg SelfSync) error {
	list, listed, err := sess.FollowStorageList(ctx, cfg.Installs, cfg.Out, cfg.Errs)
	// A relay that gave no answer is named once at the end of sync.
	if err != nil && !Unusable(err) {
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
		Now:     testNow(),
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
		as := terminal.EscapeLine(targets[i].install.As)
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
		if rep.UnnamedKept > 0 {
			fmt.Fprintf(cfg.Out, "self data: repair kept %d parts and checkpoints that no page names, because a write may still keep them: they are younger than an hour, or a write holds the lock of writes\n", rep.UnnamedKept)
		}
		if rep.UnnamedParts+rep.UnnamedCheckpoints > 0 {
			count := func(n int, one string) string {
				if n == 1 {
					return "1 " + one
				}
				return fmt.Sprintf("%d %ss", n, one)
			}
			fmt.Fprintf(cfg.Out, "self data: repair removed %s and %s that no page names, as a write that failed leaves them; the next arc sync --repair frees any that went to your storage citizens\n",
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
