package main

import (
	"bufio"
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
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/relaylist"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/bundle"
	"github.com/gezibash/arc/runtime/capability"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/gate"
	"github.com/gezibash/arc/runtime/iface"
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

	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	installs, err := installsOf(command)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	environment := []string{
		"ARC_IDENTITY=" + sess.Key.Name(),
		"ARC_PUBLIC_KEY=" + sess.Key.Public.Hex(),
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
	// provider signs it again each catalog.Refresh.
	announce := func() error {
		announcement, err := catalog.AnnounceProvider(ctx, sess.Signer, definition, nostr.Now())
		if err != nil {
			return err
		}
		_, _, err = sess.Node.Publish(ctx, announcement, sess.Relays)
		return err
	}
	if err := announce(); err != nil {
		return err
	}
	// A caller that shares no relay with this provider finds its read
	// relays in this list. An indexer that was down gets it on a later tick.
	owed := publishRelayList(ctx, sess)

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
		}, log)
	}

	// Say "serves" only when a relay has the watch, so that a caller that
	// waits for the line can call at once.
	missing, err := watchAll(ctx, server, sess.Relays, log)
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

	fmt.Fprintf(command.OutOrStdout(), "%s serves %s\n%s\n", sess.Key.Name(), id, sess.Key.Public.Hex())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	refresh := time.NewTicker(catalog.Refresh)
	defer refresh.Stop()
	for {
		select {
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
		err := server.ServeLive(ctx, r, func() {
			began = true
			report(nil)
		})
		if ctx.Err() != nil {
			return
		}
		if began {
			log.Warn("the relay ended the watch; watching again", "relay", r.Name(), "error", err)
		} else {
			report(err)
			log.Warn("the relay did not take the watch; trying again", "relay", r.Name(), "error", err)
		}
		select {
		case <-time.After(3 * time.Second):
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

func discoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover [query]",
		Short: "Find apps announced by participants",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			_, errs := sess.Node.Pull(command.Context(), announcements, sess.Relays)
			for _, err := range errs {
				fmt.Fprintf(os.Stderr, "%v\n", err)
			}

			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			offers, err := catalog.Search(sess.Node.Store, query)
			if err != nil {
				return err
			}
			if len(offers) == 0 {
				fmt.Println("no apps found: add a relay, or sync with a directory")
			}
			for _, o := range offers {
				fmt.Printf("%s/%s  %s\n  %s\n  %s\n", o.Name(), o.ID, o.Title, o.Summary, o.Provider.Hex())
			}
			return nil
		},
	}
}

func installCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "install <author-or-service> [app]",
		Short: "Install an app's commands and record your consent",
		Long: "Install the signed interface and permissions; no executable is downloaded.\n" +
			"A data app runs locally. A service app calls the selected participant.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			installs, err := installsOf(command)
			if err != nil {
				return err
			}
			provider, id, err := installs.Resolve(command.Context(), args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				id = args[1]
			}

			offer, err := sess.FindOffer(command.Context(), provider, id)
			if err != nil {
				return err
			}

			fmt.Printf("%s offers %s (%s)\n  %s\n  %s\n", offer.Name(), offer.Title, offer.ID, offer.Summary, offer.Provider.Hex())
			if m := offer.Manifest; m != nil {
				fmt.Print(iface.Describe(m))
			}
			if yes, _ := command.Flags().GetBool("yes"); !yes {
				fmt.Print("Trust this app and its signing identity? [y/N] ")
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(answer)) != "y" {
					return errors.New("not installed")
				}
			}
			if offer.Manifest == nil {
				if err := installs.Add(offer, ""); err != nil {
					return err
				}
				fmt.Printf("installed %s: call it with arc call %s\n", offer.ID, offer.Name())
				return nil
			}
			as, _ := command.Flags().GetString("as")
			if as == "" {
				as = offer.ID
			}
			if builtinName(command.Root(), as) {
				return fmt.Errorf("%s is a command of arc; choose another name with --as", as)
			}
			if err := installs.Add(offer, as); err != nil {
				return err
			}
			fmt.Printf("installed %s: see arc help %s\n", as, as)
			return nil
		},
	}
	command.Flags().Bool("yes", false, "accept the app's permissions without asking")
	command.Flags().String("as", "", "the name that runs the app (default: its id)")
	return command
}

// findOffer reads an offer from the store, and asks the relays when the store
// has none.
func callCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "call <provider|address> [body...]",
		Short: "Call an installed app service",
		Long: "Name the capability by an address, <scheme>+arc://<provider>/<path>, or\n" +
			"by its provider and --capability. The provider is a key, an npub, or an\n" +
			"installed name.\n\n" +
			"With a relay, the call is live: it needs the provider to be present\n" +
			"now, and it prints the reply and the round-trip time. With --later, or\n" +
			"with no relay, the call waits in the outbox and travels like a message;\n" +
			"arc call results shows the reply once a sync brings it.",
		Args: cobra.MinimumNArgs(1),
		RunE: callCapability,
	}
	command.Flags().Bool("later", false, "store and forward the call, even when a relay is there")
	command.Flags().String("capability", "", "the capability of the provider to call")
	command.Flags().String("method", "", "the method of the call (default: the manifest's)")
	command.Flags().String("path", "", "the path of the call (default: the manifest's)")
	command.Flags().Duration("timeout", call.Timeout, "how long a live call waits")
	command.Flags().Bool("raw", false, "write the reply as it came, not as the manifest shows it")

	command.AddCommand(&cobra.Command{
		Use: "results", Short: "Show your store-and-forward calls, and their replies", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			found := false
			now := time.Now()
			out, err := sess.Mail.Outbox(command.Context())
			if err != nil {
				return err
			}
			for _, o := range out {
				if o.Kind != "request" {
					continue
				}
				found = true
				to, _ := nostr.PubKeyFromHex(o.To)
				fmt.Printf("%s  to %s  %s\n  %s\n", o.Created.Local().Format("2006-01-02 15:04"), keys.Name(to[:]), o.State(now), o.Text)
				switch {
				case o.Reply.Err != "":
					fmt.Printf("  error: %s\n", o.Reply.Err)
				case o.ReplySeal != "":
					fmt.Printf("  reply: %s\n", o.Reply.Body)
				}
			}
			if !found {
				fmt.Println("no store-and-forward calls")
			}
			return nil
		},
	})
	return command
}

func callCapability(command *cobra.Command, args []string) error {
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()

	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	flag, _ := command.Flags().GetString("capability")
	provider, offer, addressPath, err := sess.Target(command.Context(), installs, args[0], flag)
	if err != nil {
		return err
	}
	trusted, err := installs.Trusted(provider, offer.ID)
	if err != nil {
		return err
	}
	if !trusted {
		return fmt.Errorf("install it first: arc install %s %s", provider.Hex(), offer.ID)
	}

	if !offer.SupportsInteraction(session.RequestReply) {
		return fmt.Errorf("%s does not support request/reply: %w", offer.ID, session.ErrUnsupported)
	}
	body := strings.Join(args[1:], " ")
	if body == "" {
		read, err := io.ReadAll(io.LimitReader(os.Stdin, 1024*1024+1))
		if err != nil {
			return err
		}
		body = string(read)
	}

	request := call.Request{Capability: offer.ID, Method: offer.Method, Path: offer.Path, Body: body}
	if method, _ := command.Flags().GetString("method"); method != "" {
		request.Method = strings.ToUpper(method)
	}
	if addressPath != "" {
		request.Path = addressPath
	}
	if path, _ := command.Flags().GetString("path"); path != "" {
		if addressPath != "" && path != addressPath {
			return fmt.Errorf("the address names the path %s, and --path names %s", addressPath, path)
		}
		request.Path = path
	}

	later, _ := command.Flags().GetBool("later")
	if later || len(sess.Relays) == 0 {
		if _, err := sess.Mail.Request(command.Context(), provider, request); err != nil {
			return err
		}
		fmt.Printf("queued for %s: the reply arrives with a sync; see arc call results\n", offer.Name())
		return nil
	}

	timeout, _ := command.Flags().GetDuration("timeout")
	reply, rtt, via, err := sess.LiveCall(command.Context(), provider, request, timeout)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "round trip %s via %s\n", rtt.Round(100*time.Microsecond), via)
	if reply.Err != "" {
		return fmt.Errorf("the provider refused: %s", reply.Err)
	}
	// The manifest can say how to show a reply. --raw writes it as it came.
	raw, _ := command.Flags().GetBool("raw")
	if m := offer.Manifest; !raw && m != nil && m.Service != nil && m.Service.Output != nil {
		env := &citizen.Environment{Session: sess, Installs: installs}
		in := iface.Installed{Manifest: m, Author: provider, Name: offer.ID}
		return iface.ShowReply(command.Context(), env, in, reply.Body, iface.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
	}
	fmt.Print(reply.Body)
	if !strings.HasSuffix(reply.Body, "\n") {
		fmt.Println()
	}
	return nil
}

// publishRelayList tells other citizens which relays this citizen reads and
// writes on, as NIP-65 defines, and which relays it reads its mail on, as
// NIP-17 defines. It also publishes the private relay list of NIP-37, which
// names the relays that hold the citizen's drafts. It returns the indexers
// that did not get each public list.
func publishRelayList(ctx context.Context, sess *citizen.Session) []transport.Transport {
	// The public lists go to the indexers too. The private list does not.
	type list struct {
		event nostr.Event
		to    []transport.Transport
	}
	public := slices.Concat(sess.Relays, sess.Indexers)
	var lists []list
	outbox, err := relaylist.Make(ctx, sess.Signer, sess.URLs, nostr.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "the NIP-65 relay list was not signed: %v\n", err)
	} else {
		lists = append(lists, list{outbox, public})
	}
	inbox, err := mail.RelayList(ctx, sess.Signer, sess.URLs, nostr.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "the relay list was not signed: %v\n", err)
	} else {
		lists = append(lists, list{inbox, public})
	}
	private, err := draft.RelayList(ctx, sess.Signer, sess.URLs, nostr.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "the private relay list was not signed: %v\n", err)
	} else {
		lists = append(lists, list{private, sess.Relays})
	}
	var owed []transport.Transport
	for _, l := range lists {
		_, sent, _ := sess.Node.Publish(ctx, l.event, l.to)
		for i, s := range sent {
			if s.Err != nil {
				fmt.Fprintf(os.Stderr, "the relay list did not reach %s: %v\n", s.Transport, s.Err)
				// The relays come first in l.to, and the indexers after them.
				if i >= len(sess.Relays) && !slices.Contains(owed, l.to[i]) {
					owed = append(owed, l.to[i])
				}
			}
		}
	}
	return owed
}

func announceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "announce <manifest.json>",
		Short: "Announce a capability of interface version 1 as its author",
		Long: "A data capability has no provider. Its author announces its manifest,\n" +
			"and citizens install it by trusting that author.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			announcement, err := catalog.AnnounceManifest(command.Context(), sess.Signer, data, nostr.Now())
			if err != nil {
				return err
			}
			if _, sent, err := sess.Node.Publish(command.Context(), announcement, sess.Relays); err != nil {
				return err
			} else {
				for _, s := range sent {
					if s.Err != nil {
						fmt.Fprintf(os.Stderr, "not sent to %s: %v\n", s.Transport, s.Err)
					}
				}
			}
			fmt.Printf("announced %s as %s\n%s\n", announcement.Tags.GetD(), sess.Key.Name(), sess.Key.Public.Hex())
			return nil
		},
	}
}
