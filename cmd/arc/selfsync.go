package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/spf13/cobra"
)

// selfSyncEvery is the time between two syncs of self data while arc serve
// runs. A direct path takes seconds to open, so a device never syncs after
// each write.
const selfSyncEvery = 15 * time.Minute

// selfSyncConfig is the sync of self data for the citizen of the command.
func selfSyncConfig(command *cobra.Command, sess *citizen.Session) (citizen.SelfSync, error) {
	dir, err := home(command)
	if err != nil {
		return citizen.SelfSync{}, err
	}
	root, err := rootDir(command)
	if err != nil {
		return citizen.SelfSync{}, err
	}
	installs, err := installsOf(command)
	if err != nil {
		return citizen.SelfSync{}, err
	}
	device, err := deviceKey(root)
	if err != nil {
		return citizen.SelfSync{}, fmt.Errorf("the device key: %w", err)
	}
	path, err := citizenPath(dir)
	if err != nil {
		return citizen.SelfSync{}, err
	}
	return citizen.SelfSync{
		Installs: installs,
		Device:   device,
		Dir:      filepath.Join(dir, "selfsync"),
		TURN: func(ctx context.Context) (direct.Credential, error) {
			body, err := fetchAnyTURN(ctx, dir, sess.URLs)
			if err != nil {
				return direct.Credential{}, err
			}
			var credential direct.Credential
			if err := json.Unmarshal(body, &credential); err != nil {
				return direct.Credential{}, err
			}
			return credential, nil
		},
		Options: direct.EnvOptions(sess.URLs),
		Path:    path,
		Out:     command.OutOrStdout(),
		Errs:    command.ErrOrStderr(),
	}, nil
}

// syncSelf imports the drafts of the relays once, seals old pages again
// under the data roots, and then syncs the self data with each storage
// citizen.
func syncSelf(ctx context.Context, sess *citizen.Session, cfg citizen.SelfSync, again bool) error {
	n, err := sess.ImportDrafts(ctx, cfg.Dir, again)
	if n > 0 {
		fmt.Fprintf(cfg.Out, "imported %d drafts from the relays into this machine\n", n)
	}
	if err != nil {
		fmt.Fprintf(cfg.Errs, "%v\nthe next arc sync tries again\n", err)
	}
	refreshDevices(ctx, sess, cfg.Errs)
	// Pages of the format before the data roots open only through the
	// signer. Each sync with the signer seals them again for the device.
	pages, perr := sess.ImportPages(ctx, cfg.Installs)
	if pages > 0 {
		fmt.Fprintf(cfg.Out, "sealed %d pages again under the data roots\n", pages)
	}
	if perr != nil {
		fmt.Fprintf(cfg.Errs, "pages of the old format: %v\nthe next arc sync with the signer tries again\n", perr)
	}
	return errors.Join(err, sess.SyncStorage(ctx, cfg))
}

// refreshDevices brings the device list that this machine keeps up to
// date, so that the store learns of each revocation before it takes self
// data. Without the signer, the kept list serves.
func refreshDevices(ctx context.Context, sess *citizen.Session, errs io.Writer) {
	if _, _, err := sess.DeviceList(ctx); err != nil {
		fmt.Fprintf(errs, "the device list did not come, so the list on this machine serves: %v\n", err)
	}
}

// hasStorage says if the citizen installed a storage citizen.
func hasStorage(installs catalog.Installs) bool {
	list, err := installs.List()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(list, func(i catalog.Install) bool { return i.ID == citizen.StorageApp })
}
