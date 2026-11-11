package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/host"
	"github.com/spf13/cobra"
)

// announcements matches every capability announcement.
var announcements = nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}}

func installsOf(command *cobra.Command) (catalog.Installs, error) {
	dir, err := home(command)
	if err != nil {
		return catalog.Installs{}, err
	}
	return catalog.Installs{Path: filepath.Join(dir, "installs.json")}, nil
}

func serveCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "serve <exec://...|app directory>",
		Short: "Run an app program and expose its service",
		Long: "arc announces the service interface, answers live calls that reach it\n" +
			"through a relay, and answers store-and-forward calls on each sync.\n" +
			"With --sync-dir, it also syncs with that directory on each tick, so\n" +
			"calls that couriers carry reach it.",
		Args: cobra.ExactArgs(1),
		RunE: serve,
	}
	command.Flags().StringArray("sync-dir", nil, "a directory to sync with on each tick")
	command.Flags().Duration("interval", 2*time.Second, "how often to sync")
	return command
}

func serve(command *cobra.Command, args []string) error {
	plan, err := host.Prepare(args[0])
	if err != nil {
		return err
	}
	dir, err := home(command)
	if err != nil {
		return err
	}
	release, err := host.Hold(command.ErrOrStderr(), dir, plan.ID())
	if err != nil {
		return err
	}
	defer release()

	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	relays, err := readRelays(dir)
	if err != nil {
		return err
	}
	root, err := rootDir(command)
	if err != nil {
		return err
	}
	// Self data syncs with the storage citizens once the service serves,
	// and each selfSyncEvery while it runs.
	self, err := selfSyncConfig(command, sess)
	if err != nil {
		return err
	}
	// A service reports its self sync on standard error.
	self.Out = self.Errs
	// Each device that runs arc serve repairs on its tick, and waits for
	// a direct path (decisions 4 and 7 of the storage network proposal).
	self.Repair = true
	dirs, _ := command.Flags().GetStringArray("sync-dir")
	interval, _ := command.Flags().GetDuration("interval")

	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return host.Serve(ctx, plan, host.Config{
		Session: sess, Installs: installs, Relays: relays, Root: root,
		SyncDirs: dirs, Interval: interval, SelfSyncEvery: selfSyncEvery,
		Out: command.OutOrStdout(), Errs: command.ErrOrStderr(),
		SelfSync: func(ctx context.Context) error {
			if !hasStorage(installs) {
				return nil
			}
			return syncSelf(ctx, sess, self, false)
		},
	})
}
