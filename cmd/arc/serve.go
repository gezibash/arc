package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	plan, err := prepareServe(args[0])
	if err != nil {
		return err
	}
	dir, err := home(command)
	if err != nil {
		return err
	}
	release, err := holdServe(command.ErrOrStderr(), dir, plan.definition.ID)
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
	return serveApp(ctx, plan, serveConfig{
		sess: sess, installs: installs, relays: relays, root: root,
		syncDirs: dirs, interval: interval, selfSyncEvery: selfSyncEvery,
		out: command.OutOrStdout(), errs: command.ErrOrStderr(),
		selfSync: func(ctx context.Context) error {
			if !hasStorage(installs) {
				return nil
			}
			return syncSelf(ctx, sess, self, false)
		},
	})
}

// servePlan is an app to serve: its program, and the provider definition
// and Arcfile that come with it.
type servePlan struct {
	path, cwd  string
	args       []string
	definition *capability.Provider
	held       *bundle.Bundle
}

// prepareServe reads the address of an app: exec://... or an app directory.
func prepareServe(target string) (servePlan, error) {
	address, held, err := bundle.Resolve(target)
	if err != nil {
		return servePlan{}, err
	}
	path, programArgs, manifest, cwd, err := host.ParseServeURI(address)
	if err != nil {
		return servePlan{}, err
	}
	definition, err := capability.LoadProvider(manifest)
	if err != nil {
		return servePlan{}, err
	}
	return servePlan{path: path, cwd: cwd, args: programArgs, definition: definition, held: held}, nil
}

// serveConfig is what arc serve needs of the citizen that serves.
type serveConfig struct {
	sess     *citizen.Session
	installs catalog.Installs
	// relays are the URLs of the relays of the citizen, and root is its
	// home. The program gets both in its environment.
	relays []string
	root   string
	// syncDirs are synced each interval, as relays that cannot watch are.
	syncDirs []string
	interval time.Duration
	// selfSync syncs self data, each selfSyncEvery.
	selfSync      func(context.Context) error
	selfSyncEvery time.Duration
	out, errs     io.Writer
}

// service is one arc serve as it runs.
type service struct {
	ctx       context.Context
	cfg       serveConfig
	id        string
	server    *call.Server
	announcer *announcer
	log       *slog.Logger
	// quiet logs nothing while the announcer waits.
	quiet *slog.Logger
	// mine are the announcement and relay lists of the citizen.
	mine nostr.Filter
	// watched take mail as it arrives; fetched sync each interval.
	watched []transport.Live
	fetched []transport.Transport
	// owed are the indexers that do not have the relay lists yet.
	owed []transport.Transport
}

// serveApp runs the program of the plan, and answers the calls that reach
// it, until ctx ends or the program stops.
func serveApp(ctx context.Context, plan servePlan, cfg serveConfig) error {
	log := slog.New(slog.NewTextHandler(cfg.errs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	process, program, uses, err := startProgram(ctx, plan, cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = process.Stop() }()
	s := &service{ctx: ctx, cfg: cfg, id: plan.definition.ID, log: log}
	s.server = newServer(cfg, plan.definition, program, uses, log)
	s.announcer = s.newAnnouncer(plan.definition)
	// While it waits, the line of the announcer is the only one: each
	// relay that does not answer would else warn again every 3 s (#445).
	s.quiet = slog.New(slog.NewTextHandler(waitingWriter{to: cfg.errs, waiting: s.announcer.waiting}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	stopped, err := s.start()
	if stopped || err != nil {
		return err
	}
	go s.syncSelf()
	return s.loop()
}

// startProgram starts the program, with the environment of the citizen and
// what its Arcfile allows. It returns the process, the program as callers
// reach it, and the apps that the program may call (nil for each).
func startProgram(ctx context.Context, plan servePlan, cfg serveConfig, log *slog.Logger) (*host.Process, call.Client, map[string]string, error) {
	environment := []string{
		"ARC_IDENTITY=" + cfg.sess.Key.Name(),
		"ARC_PUBLIC_KEY=" + cfg.sess.Key.Public.Hex(),
		"ARC_RELAYS=" + strings.Join(cfg.relays, " "),
		// A provider with ARC_PATH=turn runs arc relay turn as this citizen.
		"ARC_HOME=" + cfg.root,
	}
	if program, err := os.Executable(); err == nil {
		environment = append(environment, "ARC_PROGRAM="+program)
	}
	// The Arcfile can limit who reaches the program, and what it calls. Both
	// resolve once, at the start: a change needs a restart.
	var allowed map[string]bool
	var uses map[string]string
	if plan.held != nil {
		var err error
		if allowed, err = allowList(ctx, cfg.installs, plan.held.Allow); err != nil {
			return nil, nil, nil, err
		}
		var useEnv []string
		if uses, useEnv, err = useList(cfg.installs, plan.held.Uses); err != nil {
			return nil, nil, nil, err
		}
		environment = append(environment, useEnv...)
		if allowed != nil {
			log.Info("the Arcfile allows only some callers", "callers", len(allowed))
		}
		if uses != nil {
			log.Info("the Arcfile limits the calls of the program", "uses", useEnv)
		}
	}
	process, err := host.Start(plan.path, plan.args, plan.cwd, environment, log)
	if err != nil {
		return nil, nil, nil, err
	}
	var program call.Client = process
	if allowed != nil {
		program = gate.New(process, func(from string) bool { return allowed[from] })
	}
	return process, program, uses, nil
}

// newServer answers the calls that reach the program. The program can call
// what this citizen installed, as this citizen. [uses] in the Arcfile
// narrows that to the apps that it names.
func newServer(cfg serveConfig, definition *capability.Provider, program call.Client, uses map[string]string, log *slog.Logger) *call.Server {
	sess, installs := cfg.sess, cfg.installs
	calls := func(ctx context.Context, out call.Outbound) (call.Reply, error) {
		if err := inUses(ctx, installs, uses, out.Address); err != nil {
			return call.Reply{}, err
		}
		return sess.CallAddress(ctx, installs, out.Address, out.Body)
	}
	server := call.NewServer(sess.Signer, definition.ID, program, definition.MaxBytes, calls, log, definition.Interactions...)
	server.SetSessionCaller(func(ctx context.Context, out call.Outbound, mode session.Mode) (*session.Stream, error) {
		if err := inUses(ctx, installs, uses, out.Address); err != nil {
			return nil, err
		}
		return sess.OpenSessionAddress(ctx, installs, out.Address, out.Body, mode)
	})
	sess.Mail.OnRequest = server.Handle
	sess.Mail.Serves = server.Serves
	return server
}

// newAnnouncer signs the announcement of the provider. A caller needs a
// current announcement before a live call, so the provider signs it again
// each catalog.Refresh. Each one is at least a second newer than the newest
// that this machine knows, so that it never ties with an older one of the
// same second (#422). Until a relay answers for the announcements that the
// relays hold, arc cannot know the newest, and signs nothing: it says so
// once, and asks again.
func (s *service) newAnnouncer(definition *capability.Provider) *announcer {
	ctx, sess := s.ctx, s.cfg.sess
	announced := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{sess.Key.Public}, Tags: nostr.TagMap{"d": {definition.ID}}}
	a := &announcer{
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
			fmt.Fprintln(s.cfg.errs, "arc: info: "+terminal.EscapeLine(fmt.Sprintf("%s %s", definition.ID, text)))
		},
	}
	a.known.Store(len(sess.Relays) == 0)
	return a
}

// start announces the service, starts the watches, and says "serves". It
// returns true when ctx ended first.
func (s *service) start() (bool, error) {
	ctx, sess := s.ctx, s.cfg.sess
	if err := s.announcer.announce(); err != nil {
		return false, err
	}
	// A caller that shares no relay with this provider finds its read
	// relays in this list. An indexer that was down gets it on a later tick.
	s.owed = publishRelayList(ctx, sess, s.cfg.errs, s.announcer.waiting)

	// A watch takes the mail of a live relay as it arrives. A tick syncs a
	// directory, or a relay that cannot watch, because neither can push.
	s.mine = nostr.Filter{Kinds: []nostr.Kind{catalog.Kind, relaylist.Kind, mail.RelayListKind}, Authors: []nostr.PubKey{sess.Key.Public}}
	for _, t := range sess.Relays {
		if live, ok := t.(transport.Live); ok {
			s.watched = append(s.watched, live)
		} else {
			s.fetched = append(s.fetched, t)
		}
	}
	for _, dir := range s.cfg.syncDirs {
		s.fetched = append(s.fetched, file.Dir{Path: dir})
	}
	for _, r := range s.watched {
		// A relay that was down gets the announcement and the relay lists
		// again when its watch begins again.
		go keepMail(ctx, sess.Mail, r, func() {
			if _, err := sess.Node.Sync(ctx, s.mine, r); err != nil {
				s.log.Debug("the announcement did not sync", "transport", r.Name(), "error", err)
			}
		}, s.quiet)
	}

	// Say "serves" only when a relay has the watch, so that a caller that
	// waits for the line can call at once.
	missing, err := watchAll(ctx, s.server, sess.Relays, s.quiet)
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "the service is stopping")
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if len(missing) > 0 {
		s.log.Warn("serves without these relays until they take the watch", "relays", missing)
		// A relay that took the watch late did not get the announcement.
		if err := s.announcer.announce(); err != nil {
			s.log.Warn("the announcement was not signed again", "error", err)
		}
	}

	fmt.Fprintf(s.cfg.out, "%s serves %s\n%s\n", sess.Key.Name(), s.id, sess.Key.Public.Hex()) //arc:raw-ok the name of a key of this machine
	return false, nil
}

// syncSelf syncs self data now, and each selfSyncEvery until ctx ends.
func (s *service) syncSelf() {
	tick := time.NewTicker(s.cfg.selfSyncEvery)
	defer tick.Stop()
	for {
		if err := s.cfg.selfSync(s.ctx); err != nil && s.ctx.Err() == nil {
			s.log.Warn("self data did not sync", "error", err)
		}
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// loop announces again, and syncs what cannot watch, until ctx ends or the
// program stops.
func (s *service) loop() error {
	ticker := time.NewTicker(s.cfg.interval)
	defer ticker.Stop()
	refresh := time.NewTicker(catalog.Refresh)
	defer refresh.Stop()
	for {
		// While no relay answered, the announcement waits: ask again soon.
		var unknown <-chan time.Time
		if s.announcer.waiting() {
			unknown = time.After(3 * time.Second)
		}
		select {
		case <-unknown:
			if err := s.announcer.announce(); err != nil {
				s.log.Warn("the announcement was not signed", "error", err)
			}
		case <-s.ctx.Done():
			fmt.Fprintln(os.Stderr, "the service is stopping")
			return nil
		case <-s.server.Done():
			return errors.New("the app program stopped")
		case <-refresh.C:
			if err := s.announcer.announce(); err != nil {
				s.log.Warn("the announcement was not signed again", "error", err)
			}
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick syncs each transport that cannot watch, sends the waiting messages,
// and flushes the mail of each watched relay.
func (s *service) tick() {
	ctx, sess := s.ctx, s.cfg.sess
	// The sync sends the relay lists too, so a directory gets them.
	for _, t := range s.fetched {
		if _, err := sess.Node.Sync(ctx, s.mine, t); err != nil {
			s.log.Debug("the announcement did not sync", "transport", t.Name(), "error", err)
		}
		report, err := sess.Mail.Sync(ctx, t)
		if err != nil {
			s.log.Debug("mail did not sync", "transport", t.Name(), "error", err)
			continue
		}
		if report.Answered > 0 {
			s.log.Info("answered store-and-forward calls", "transport", t.Name(), "calls", report.Answered)
		}
	}
	if sent, err := sess.Mail.Forward(ctx); err != nil {
		s.log.Warn("the waiting messages were not sent to the relays of their recipients", "error", err)
	} else if sent > 0 {
		s.log.Info("sent waiting messages to relays that their recipients read", "wraps", sent)
	}
	// A watched relay needs only a flush. It sends nothing when the
	// relay holds each wrap, so an idle provider stays quiet.
	for _, r := range s.watched {
		report, err := sess.Mail.Flush(ctx, r)
		if err != nil {
			s.log.Debug("mail did not flush", "transport", r.Name(), "error", err)
			continue
		}
		if report.Answered > 0 {
			s.log.Info("answered store-and-forward calls", "transport", r.Name(), "calls", report.Answered)
		}
	}
	// An indexer holds only relay lists, so it gets a sync only
	// until it has them.
	lists := nostr.Filter{Kinds: []nostr.Kind{relaylist.Kind, mail.RelayListKind}, Authors: []nostr.PubKey{sess.Key.Public}}
	s.owed = slices.DeleteFunc(s.owed, func(t transport.Transport) bool {
		report, err := sess.Node.Sync(ctx, lists, t)
		return err == nil && len(report.SendFailed) == 0
	})
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
