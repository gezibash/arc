package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/provider/host"
	"github.com/gezibash/arc/adapters/transport/file"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/relaylist"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/bundle"
	"github.com/gezibash/arc/runtime/capability"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/gate"
	"github.com/gezibash/arc/sdk/terminal"
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
	address, held, err := bundle.Resolve(args[0])
	if err != nil {
		return err
	}
	path, programArgs, manifest, cwd, err := host.ParseServeURI(address)
	if err != nil {
		return err
	}
	definition, err := capability.LoadProvider(manifest)
	if err != nil {
		return err
	}
	id, limit := definition.ID, definition.MaxBytes
	dir, err := home(command)
	if err != nil {
		return err
	}
	release, err := holdServe(command.ErrOrStderr(), dir, id)
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

	log := slog.New(slog.NewTextHandler(command.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	relays, err := readRelays(dir)
	if err != nil {
		return err
	}
	root, err := rootDir(command)
	if err != nil {
		return err
	}
	environment := []string{
		"ARC_IDENTITY=" + sess.Key.Name(),
		"ARC_PUBLIC_KEY=" + sess.Key.Public.Hex(),
		"ARC_RELAYS=" + strings.Join(relays, " "),
		// A provider with ARC_PATH=turn runs arc relay turn as this citizen.
		"ARC_HOME=" + root,
	}
	if program, err := os.Executable(); err == nil {
		environment = append(environment, "ARC_PROGRAM="+program)
	}
	// The Arcfile can limit who reaches the program, and what it calls. Both
	// resolve once, at the start: a change needs a restart.
	var allowed map[string]bool
	var uses map[string]string
	if held != nil {
		if allowed, err = allowList(command.Context(), installs, held.Allow); err != nil {
			return err
		}
		var useEnv []string
		if uses, useEnv, err = useList(installs, held.Uses); err != nil {
			return err
		}
		environment = append(environment, useEnv...)
		if allowed != nil {
			log.Info("the Arcfile allows only some callers", "callers", len(allowed))
		}
		if uses != nil {
			log.Info("the Arcfile limits the calls of the program", "uses", useEnv)
		}
	}
	process, err := host.Start(path, programArgs, cwd, environment, log)
	if err != nil {
		return err
	}
	defer func() { _ = process.Stop() }()
	var program call.Client = process
	if allowed != nil {
		program = gate.New(process, func(from string) bool { return allowed[from] })
	}

	// The program can call what this citizen installed, as this citizen.
	// [uses] in the Arcfile narrows that to the apps that it names.
	calls := func(ctx context.Context, out call.Outbound) (call.Reply, error) {
		if err := inUses(ctx, installs, uses, out.Address); err != nil {
			return call.Reply{}, err
		}
		return sess.CallAddress(ctx, installs, out.Address, out.Body)
	}
	server := call.NewServer(sess.Signer, id, program, limit, calls, log, definition.Interactions...)
	server.SetSessionCaller(func(ctx context.Context, out call.Outbound, mode session.Mode) (*session.Stream, error) {
		if err := inUses(ctx, installs, uses, out.Address); err != nil {
			return nil, err
		}
		return sess.OpenSessionAddress(ctx, installs, out.Address, out.Body, mode)
	})
	sess.Mail.OnRequest = server.Handle
	sess.Mail.Serves = server.Serves

	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A caller needs a current announcement before a live call, so the
	// provider signs it again each catalog.Refresh. Each one is at least a
	// second newer than the newest that this machine knows, so that it
	// never ties with an older one of the same second (#422). Until a relay
	// answers for the announcements that the relays hold, arc cannot know
	// the newest, and signs nothing: it says so once, and asks again.
	announced := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{sess.Key.Public}, Tags: nostr.TagMap{"d": {definition.ID}}}
	gate := &announcer{
		pull: func() error { return node.Unreached(sess.Node.Pull(ctx, announced, sess.Relays)) },
		at: func() (nostr.Timestamp, error) {
			return catalog.After(sess.Node.Store, sess.Key.Public, definition.ID, nostr.Now())
		},
		sign: func(at nostr.Timestamp) error {
			announcement, err := catalog.AnnounceProvider(ctx, sess.Signer, definition, at)
			if err != nil {
				return err
			}
			_, _, err = sess.Node.Publish(ctx, announcement, sess.Relays)
			return err
		},
		say: func(text string) {
			fmt.Fprintln(command.ErrOrStderr(), "arc: info: "+terminal.EscapeLine(fmt.Sprintf("%s %s", definition.ID, text)))
		},
	}
	gate.known.Store(len(sess.Relays) == 0)
	announce := gate.announce
	// While it waits, the line of the announcer is the only one: each
	// relay that does not answer would else warn again every 3 s (#445).
	quiet := slog.New(slog.NewTextHandler(waitingWriter{to: command.ErrOrStderr(), waiting: gate.waiting}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := announce(); err != nil {
		return err
	}
	// A caller that shares no relay with this provider finds its read
	// relays in this list. An indexer that was down gets it on a later tick.
	owed := publishRelayList(ctx, sess, command.ErrOrStderr(), gate.waiting)

	// A watch takes the mail of a live relay as it arrives. A tick syncs a
	// directory, or a relay that cannot watch, because neither can push.
	mine := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind, relaylist.Kind, mail.RelayListKind}, Authors: []nostr.PubKey{sess.Key.Public}}
	dirs, _ := command.Flags().GetStringArray("sync-dir")
	interval, _ := command.Flags().GetDuration("interval")
	var watched []transport.Live
	var fetched []transport.Transport
	for _, t := range sess.Relays {
		if live, ok := t.(transport.Live); ok {
			watched = append(watched, live)
		} else {
			fetched = append(fetched, t)
		}
	}
	for _, dir := range dirs {
		fetched = append(fetched, file.Dir{Path: dir})
	}
	for _, r := range watched {
		// A relay that was down gets the announcement and the relay lists
		// again when its watch begins again.
		go keepMail(ctx, sess.Mail, r, func() {
			if _, err := sess.Node.Sync(ctx, mine, r); err != nil {
				log.Debug("the announcement did not sync", "transport", r.Name(), "error", err)
			}
		}, quiet)
	}

	// Say "serves" only when a relay has the watch, so that a caller that
	// waits for the line can call at once.
	missing, err := watchAll(ctx, server, sess.Relays, quiet)
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "the service is stopping")
		return nil
	}
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		log.Warn("serves without these relays until they take the watch", "relays", missing)
		// A relay that took the watch late did not get the announcement.
		if err := announce(); err != nil {
			log.Warn("the announcement was not signed again", "error", err)
		}
	}

	fmt.Fprintf(command.OutOrStdout(), "%s serves %s\n%s\n", sess.Key.Name(), id, sess.Key.Public.Hex()) //arc:raw-ok the name of a key of this machine

	// Self data syncs with the storage citizens now, and each
	// selfSyncEvery while the service runs.
	self, err := selfSyncConfig(command, sess)
	if err != nil {
		return err
	}
	// A service reports its self sync on standard error.
	self.Out = self.Errs
	// Each device that runs arc serve repairs on its tick, and waits for
	// a direct path (decisions 4 and 7 of the storage network proposal).
	self.Repair = true
	go func() {
		tick := time.NewTicker(selfSyncEvery)
		defer tick.Stop()
		for {
			if hasStorage(installs) {
				if err := syncSelf(ctx, sess, self, false); err != nil && ctx.Err() == nil {
					log.Warn("self data did not sync", "error", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	refresh := time.NewTicker(catalog.Refresh)
	defer refresh.Stop()
	for {
		// While no relay answered, the announcement waits: ask again soon.
		var unknown <-chan time.Time
		if gate.waiting() {
			unknown = time.After(3 * time.Second)
		}
		select {
		case <-unknown:
			if err := announce(); err != nil {
				log.Warn("the announcement was not signed", "error", err)
			}
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "the service is stopping")
			return nil
		case <-server.Done():
			return errors.New("the app program stopped")
		case <-refresh.C:
			if err := announce(); err != nil {
				log.Warn("the announcement was not signed again", "error", err)
			}
		case <-ticker.C:
			// The sync sends the relay lists too, so a directory gets them.
			for _, t := range fetched {
				if _, err := sess.Node.Sync(ctx, mine, t); err != nil {
					log.Debug("the announcement did not sync", "transport", t.Name(), "error", err)
				}
				report, err := sess.Mail.Sync(ctx, t)
				if err != nil {
					log.Debug("mail did not sync", "transport", t.Name(), "error", err)
					continue
				}
				if report.Answered > 0 {
					log.Info("answered store-and-forward calls", "transport", t.Name(), "calls", report.Answered)
				}
			}
			if sent, err := sess.Mail.Forward(ctx); err != nil {
				log.Warn("the waiting messages were not sent to the relays of their recipients", "error", err)
			} else if sent > 0 {
				log.Info("sent waiting messages to relays that their recipients read", "wraps", sent)
			}
			// A watched relay needs only a flush. It sends nothing when the
			// relay holds each wrap, so an idle provider stays quiet.
			for _, r := range watched {
				report, err := sess.Mail.Flush(ctx, r)
				if err != nil {
					log.Debug("mail did not flush", "transport", r.Name(), "error", err)
					continue
				}
				if report.Answered > 0 {
					log.Info("answered store-and-forward calls", "transport", r.Name(), "calls", report.Answered)
				}
			}
			// An indexer holds only relay lists, so it gets a sync only
			// until it has them.
			lists := nostr.Filter{Kinds: []nostr.Kind{relaylist.Kind, mail.RelayListKind}, Authors: []nostr.PubKey{sess.Key.Public}}
			owed = slices.DeleteFunc(owed, func(t transport.Transport) bool {
				report, err := sess.Node.Sync(ctx, lists, t)
				return err == nil && len(report.SendFailed) == 0
			})
		}
	}
}

// allowList resolves allow of the Arcfile to public keys in hex. An entry
// takes any form of a key that a command takes. Nil allows every caller. An
// entry that does not resolve stops the start, so the list fails closed.
func allowList(ctx context.Context, installs catalog.Installs, entries []string) (map[string]bool, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	env := &citizen.Environment{Installs: installs}
	allowed := map[string]bool{}
	for _, entry := range entries {
		pk, err := env.ResolveKey(ctx, entry)
		if err != nil {
			return nil, fmt.Errorf("allow in the Arcfile: %q is not a key: %w", entry, err)
		}
		allowed[pk.Hex()] = true
	}
	return allowed, nil
}

// useList resolves [uses] of the Arcfile to the installed apps. It returns
// the apps as "<id> <key>", and the environment that names each address.
// Nil allows calls to every installed app.
func useList(installs catalog.Installs, uses []bundle.Use) (map[string]string, []string, error) {
	if uses == nil {
		return nil, nil, nil
	}
	apps := map[string]string{}
	var env []string
	for _, use := range uses {
		app, ok, err := installs.Named(use.Installed)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("[uses] in the Arcfile: %s names %q, which this citizen has not installed: run arc install <key> --as %s", use.Name, use.Installed, use.Installed)
		}
		apps[app.ID+" "+app.Provider] = use.Name
		env = append(env, use.Env()+"="+app.ID+"+arc://"+app.Provider+"/")
	}
	return apps, env, nil
}

// inUses refuses a call of the program to an app that [uses] does not name.
func inUses(ctx context.Context, installs catalog.Installs, uses map[string]string, address string) error {
	if uses == nil {
		return nil
	}
	parsed, err := catalog.ParseAddress(address)
	if err != nil {
		return err
	}
	pk, _, err := installs.Resolve(ctx, parsed.Provider)
	if err != nil {
		return err
	}
	if _, ok := uses[parsed.Scheme+" "+pk.Hex()]; !ok {
		return fmt.Errorf("not_in_uses: %s is not in [uses] of the Arcfile", address)
	}
	return nil
}

// watchAll starts a live watch on each relay. It returns when each relay
// has begun or failed its first watch, and at least one relay has the watch.
// If no relay has the watch, watchAll waits until one relay takes it. It
// returns the relays whose first watch failed. Those relays get the watch
// when they come back.
func watchAll(ctx context.Context, server *call.Server, relays []transport.Transport, log *slog.Logger) ([]string, error) {
	type result struct {
		index int
		err   error
	}
	results := make(chan result)
	started := make(chan struct{})
	defer close(started)
	count := 0
	var missing []string
	for i, t := range relays {
		live, ok := t.(transport.Live)
		if !ok {
			missing = append(missing, t.Name())
			continue
		}
		count++
		go keepServing(ctx, server, live, func(err error) {
			select {
			case results <- result{i, err}:
			case <-started:
			case <-ctx.Done():
			}
		}, log)
	}

	answered := map[int]bool{}
	watches := 0
	for len(answered) < count || (watches == 0 && count > 0) {
		select {
		case r := <-results:
			if r.err == nil {
				watches++
			} else if !answered[r.index] {
				missing = append(missing, relays[r.index].Name())
			}
			answered[r.index] = true
		case <-server.Done():
			return nil, errors.New("the app program stopped")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return missing, nil
}

// keepServing answers live calls through one relay, and watches again when
// the relay drops the connection. After each attempt to watch, it calls
// report: with nil when the watch begins, and with the error when the watch
// does not begin.
func keepServing(ctx context.Context, server *call.Server, r transport.Live, report func(error), log *slog.Logger) {
	for {
		began := false
		var start time.Time
		err := server.ServeLive(ctx, r, func() {
			began, start = true, time.Now()
			report(nil)
		})
		if ctx.Err() != nil {
			return
		}
		// While no watch runs, each call to this provider reaches no one,
		// so a watch that ran a while starts again at once (#424). A watch
		// that ends at once waits, so that a relay that ends each watch
		// does not get a new one in a loop.
		wait := 3 * time.Second
		if began {
			log.Warn("the relay ended the watch; watching again", "relay", r.Name(), "error", err)
			if time.Since(start) > 10*time.Second {
				wait = 0
			}
		} else {
			report(err)
			log.Warn("the relay did not take the watch; trying again", "relay", r.Name(), "error", err)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// keepMail watches the mail of one relay, and watches again when the relay
// drops the connection. A watch that ends at midnight starts again at once,
// with the route tags of the new day.
func keepMail(ctx context.Context, box *mail.Mail, r transport.Live, ready func(), log *slog.Logger) {
	for {
		err := box.Watch(ctx, r, ready)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue
		}
		log.Warn("the mail watch ended; watching again", "relay", r.Name(), "error", err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
