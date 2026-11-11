// Package host serves one app for its citizen: it runs the program of the
// app, announces it, and answers the calls that reach it, live and by mail.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	hostadapter "github.com/gezibash/arc/adapters/provider/host"
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
)

// Plan is an app to serve: its program, and the provider definition
// and Arcfile that come with it.
type Plan struct {
	path, cwd  string
	args       []string
	definition *capability.Provider
	held       *bundle.Bundle
}

// ID is the capability that the app serves.
func (p Plan) ID() string { return p.definition.ID }

// Prepare reads the address of an app: exec://... or an app directory.
func Prepare(target string) (Plan, error) {
	address, held, err := bundle.Resolve(target)
	if err != nil {
		return Plan{}, err
	}
	path, programArgs, manifest, cwd, err := hostadapter.ParseServeURI(address)
	if err != nil {
		return Plan{}, err
	}
	definition, err := capability.LoadProvider(manifest)
	if err != nil {
		return Plan{}, err
	}
	return Plan{path: path, cwd: cwd, args: programArgs, definition: definition, held: held}, nil
}

// Config is what Serve needs of the citizen that serves.
type Config struct {
	Session  *citizen.Session
	Installs catalog.Installs
	// Relays are the URLs of the relays of the citizen, and Root is its
	// home. The program gets both in its environment.
	Relays []string
	Root   string
	// SyncDirs are synced each Interval, as relays that cannot watch are.
	SyncDirs []string
	Interval time.Duration
	// SelfSync syncs self data, each SelfSyncEvery.
	SelfSync      func(context.Context) error
	SelfSyncEvery time.Duration
	Out, Errs     io.Writer
}

// service is one arc serve as it runs.
type service struct {
	ctx       context.Context
	cfg       Config
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

// Serve runs the program of the plan, and answers the calls that reach
// it, until ctx ends or the program stops.
func Serve(ctx context.Context, plan Plan, cfg Config) error {
	log := slog.New(slog.NewTextHandler(cfg.Errs, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
	s.quiet = slog.New(slog.NewTextHandler(waitingWriter{to: cfg.Errs, waiting: s.announcer.waiting}, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
func startProgram(ctx context.Context, plan Plan, cfg Config, log *slog.Logger) (*hostadapter.Process, call.Client, map[string]string, error) {
	environment := []string{
		"ARC_IDENTITY=" + cfg.Session.Key.Name(),
		"ARC_PUBLIC_KEY=" + cfg.Session.Key.Public.Hex(),
		"ARC_RELAYS=" + strings.Join(cfg.Relays, " "),
		// A provider with ARC_PATH=turn runs arc relay turn as this citizen.
		"ARC_HOME=" + cfg.Root,
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
		if allowed, err = allowList(ctx, cfg.Installs, plan.held.Allow); err != nil {
			return nil, nil, nil, err
		}
		var useEnv []string
		if uses, useEnv, err = useList(cfg.Installs, plan.held.Uses); err != nil {
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
	process, err := hostadapter.Start(plan.path, plan.args, plan.cwd, environment, log)
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
func newServer(cfg Config, definition *capability.Provider, program call.Client, uses map[string]string, log *slog.Logger) *call.Server {
	sess, installs := cfg.Session, cfg.Installs
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
	ctx, sess := s.ctx, s.cfg.Session
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
			fmt.Fprintln(s.cfg.Errs, "arc: info: "+terminal.EscapeLine(fmt.Sprintf("%s %s", definition.ID, text)))
		},
	}
	a.known.Store(len(sess.Relays) == 0)
	return a
}

// start announces the service, starts the watches, and says "serves". It
// returns true when ctx ended first.
func (s *service) start() (bool, error) {
	ctx, sess := s.ctx, s.cfg.Session
	if err := s.announcer.announce(); err != nil {
		return false, err
	}
	// A caller that shares no relay with this provider finds its read
	// relays in this list. An indexer that was down gets it on a later tick.
	s.owed = sess.PublishRelayList(ctx, s.cfg.Errs, s.announcer.waiting)

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
	for _, dir := range s.cfg.SyncDirs {
		s.fetched = append(s.fetched, file.Dir{Path: dir})
	}
	for _, r := range s.watched {
		// A relay that was down gets the announcement and the relay lists
		// again when its watch begins again.
		go sess.KeepMail(ctx, r, func() {
			if _, err := sess.Node.Sync(ctx, s.mine, r); err != nil {
				s.log.Debug("the announcement did not sync", "transport", r.Name(), "error", err)
			}
		}, s.quiet)
	}

	// Say "serves" only when a relay has the watch, so that a caller that
	// waits for the line can call at once.
	missing, err := watchAll(ctx, s.server, sess.Relays, s.quiet)
	if ctx.Err() != nil {
		fmt.Fprintln(s.cfg.Errs, "the service is stopping")
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

	fmt.Fprintf(s.cfg.Out, "%s serves %s\n%s\n", sess.Key.Name(), s.id, sess.Key.Public.Hex()) //arc:raw-ok the name of a key of this machine
	return false, nil
}

// syncSelf syncs self data now, and each SelfSyncEvery until ctx ends.
func (s *service) syncSelf() {
	tick := time.NewTicker(s.cfg.SelfSyncEvery)
	defer tick.Stop()
	for {
		if err := s.cfg.SelfSync(s.ctx); err != nil && s.ctx.Err() == nil {
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
	ticker := time.NewTicker(s.cfg.Interval)
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
			fmt.Fprintln(s.cfg.Errs, "the service is stopping")
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
	ctx, sess := s.ctx, s.cfg.Session
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
