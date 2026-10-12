package citizen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/node"
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
	// Out gets one line for each storage citizen. Errs gets the notes of
	// the pipe and each refused blob.
	Out, Errs io.Writer
}

// sealSelf is the one place that chooses the keys of the blobs. Today the
// keys of generation 0 follow from the keyed root, which each holder of
// the citizen key derives. Base step 2 (random data roots) replaces this
// function.
func (sess *Session) sealSelf(ctx context.Context, gen uint32) (selfsync.Keys, error) {
	if gen != 0 {
		return selfsync.Keys{}, fmt.Errorf("the blob is of generation %d, and this machine knows only generation 0: update arc", gen)
	}
	root, err := sess.KeyedRoot(ctx)
	if err != nil {
		return selfsync.Keys{}, err
	}
	return selfsync.KeysFromRoot(root)
}

// importedFile marks that this machine imported the drafts of the relays.
const importedFile = "imported"

// ImportDrafts gets the self data of this citizen from each relay, and
// keeps it in the local store, once. The drafts on the relays stay there.
// A second run gets nothing new: the store keeps each event once. With
// again, it runs even if it ran before.
func (sess *Session) ImportDrafts(ctx context.Context, dir string, again bool) (int, error) {
	marker := filepath.Join(dir, importedFile)
	if _, err := os.Stat(marker); err == nil && !again {
		return 0, nil
	}
	if len(sess.Relays) == 0 {
		return 0, nil
	}
	reports, errs := sess.Node.Pull(ctx, selfsync.Filter(sess.Key.Public), sess.Relays)
	received := 0
	for _, r := range reports {
		received += r.Received
	}
	if err := node.Unreached(reports, errs); err != nil {
		return received, fmt.Errorf("the import of the drafts on the relays is not complete: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return received, err
	}
	return received, os.WriteFile(marker, []byte(nostr.Now().Time().UTC().Format("2006-01-02T15:04:05Z")+"\n"), 0o600)
}

// SyncStorage syncs the self data of this machine with each storage
// citizen that the citizen installed. With none, the data stays on this
// machine, and it says so.
func (sess *Session) SyncStorage(ctx context.Context, cfg SelfSync) error {
	installs, err := cfg.Installs.List()
	if err != nil {
		return err
	}
	var targets []catalog.Install
	for _, install := range installs {
		if install.ID == StorageApp && install.As != "" {
			targets = append(targets, install)
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
	syncer := &selfsync.Syncer{
		Citizen: sess.Key.Public,
		Device:  signer,
		Store:   sess.Node.Store,
		Keys:    sess.sealSelf,
		Devices: func(ctx context.Context) (devices.List, bool, error) { return sess.DeviceList(ctx) },
		State:   cfg.Dir,
	}
	var failed []error
	for _, install := range targets {
		report, err := sess.syncOne(ctx, cfg, syncer, install)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", install.As, err))
			fmt.Fprintf(cfg.Errs, "%s: self data: %v\n", install.As, err)
			continue
		}
		fmt.Fprintf(cfg.Out, "%s: self data received %d, sent %d, already held %d\n", install.As, report.Received, report.Sent, report.Held)
		for _, reason := range report.Refused {
			fmt.Fprintf(cfg.Errs, "  refused %s\n", reason)
		}
	}
	return errors.Join(failed...)
}

func (sess *Session) syncOne(ctx context.Context, cfg SelfSync, syncer *selfsync.Syncer, install catalog.Install) (selfsync.Report, error) {
	client := storage.Client{
		Call: func(ctx context.Context, session string) ([]byte, error) {
			return sess.openStorage(ctx, cfg.Installs, install.As, session)
		},
		TURN: cfg.TURN, Options: cfg.Options, Notes: cfg.Errs,
	}
	conn, err := client.Open(ctx, 0)
	if err != nil {
		return selfsync.Report{}, err
	}
	defer conn.Close()
	return syncer.Sync(ctx, install.Provider, conn)
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
		switch {
		case strings.Contains(text, "not_owner"):
			return nil, errors.New("the storage citizen refused this citizen: it is not an owner of the account")
		case strings.Contains(text, "busy"):
			return nil, errors.New("the storage citizen runs too many sessions now; try again later")
		case text != "":
			return nil, fmt.Errorf("the ARC call failed: %s", text)
		}
		return nil, fmt.Errorf("the ARC call failed: %w", err)
	}
	return bytes.TrimSpace(out.Bytes()), nil
}
