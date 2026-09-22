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
	"strings"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/bundle"
	"github.com/gezibash/arc/capability"
	"github.com/gezibash/arc/citizen"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/draft"
	"github.com/gezibash/arc/delivery/mail"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/iface"
	"github.com/gezibash/arc/provider/host"
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
		Use:   "serve <exec://...|bundle directory>",
		Short: "Offer a capability, and answer its calls",
		Long: "arcn announces the capability, answers live calls that reach it\n" +
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
	address, _, err := bundle.Resolve(args[0])
	if err != nil {
		return err
	}
	path, programArgs, manifest, err := citizen.ParseServeURI(address)
	if err != nil {
		return err
	}
	pkg, err := capability.LoadFile(manifest)
	if err != nil {
		return err
	}
	fields, _ := pkg["capability"].(map[string]any)
	id, _ := fields["id"].(string)
	limit := maxBody(fields)

	// A manifest of interface version 1 beside the older one replaces it in
	// the announcement.
	versionOne, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), "interface.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if versionOne != nil {
		m, err := iface.Parse(versionOne)
		if err != nil {
			return err
		}
		id = m.ID
		if m.Service != nil && m.Service.MaxBytes > 0 {
			limit = m.Service.MaxBytes
		}
	}

	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	process, err := host.Start(path, programArgs, []string{
		"ARC_IDENTITY=" + sess.key.Name(),
		"ARC_PUBLIC_KEY=" + sess.key.Public.Hex(),
	}, log)
	if err != nil {
		return err
	}
	defer process.Stop()

	server := call.NewServer(sess.signer, id, process, limit, log)
	sess.mail.OnRequest = server.Handle

	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	announcement, err := catalog.Announce(sess.signer, pkg, nostr.Now())
	if versionOne != nil {
		announcement, err = catalog.AnnounceManifest(sess.signer, versionOne, nostr.Now())
	}
	if err != nil {
		return err
	}
	if _, _, err := sess.node.Publish(ctx, announcement, sess.relays); err != nil {
		return err
	}

	for _, r := range sess.relays {
		go keepServing(ctx, server, r.(relay.Relay), log)
	}

	dirs, _ := command.Flags().GetStringArray("sync-dir")
	interval, _ := command.Flags().GetDuration("interval")
	targets := append([]transport.Transport(nil), sess.relays...)
	for _, dir := range dirs {
		targets = append(targets, file.Dir{Path: dir})
	}

	fmt.Printf("%s serves %s\n%s\n", sess.key.Name(), id, sess.key.Public.Hex())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "the provider is stopping")
			return nil
		case <-server.Done():
			return errors.New("the provider program stopped")
		case <-ticker.C:
			for _, t := range targets {
				mine := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{sess.key.Public}}
				if _, err := sess.node.Sync(ctx, mine, t); err != nil {
					log.Debug("the announcement did not sync", "transport", t.Name(), "error", err)
				}
				report, err := sess.mail.Sync(ctx, t)
				if err != nil {
					log.Debug("mail did not sync", "transport", t.Name(), "error", err)
					continue
				}
				if report.Answered > 0 {
					log.Info("answered store-and-forward calls", "transport", t.Name(), "calls", report.Answered)
				}
			}
		}
	}
}

// keepServing answers live calls through one relay, and watches again when
// the relay drops the connection.
func keepServing(ctx context.Context, server *call.Server, r relay.Relay, log *slog.Logger) {
	for {
		err := server.ServeLive(ctx, r)
		if ctx.Err() != nil {
			return
		}
		log.Warn("the relay ended the watch; watching again", "relay", r.URL, "error", err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// maxBody is the largest request body that the manifest allows.
func maxBody(fields map[string]any) int {
	invocation, _ := fields["invocation"].(map[string]any)
	body, _ := invocation["request_body"].(map[string]any)
	switch v := body["max_bytes"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	if n, ok := body["max_bytes"].(interface{ Int64() (int64, error) }); ok {
		if value, err := n.Int64(); err == nil {
			return int(value)
		}
	}
	return 1024 * 1024
}

func discoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover [query]",
		Short: "Find capabilities that providers announce",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			_, errs := sess.node.Pull(command.Context(), announcements, sess.relays)
			for _, err := range errs {
				fmt.Fprintf(os.Stderr, "%v\n", err)
			}

			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			offers := catalog.Search(sess.node.Store, query)
			if len(offers) == 0 {
				fmt.Println("no capabilities found: add a relay, or sync with a directory")
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
		Use:   "install <provider> [capability]",
		Short: "Trust a capability, so that you can call it",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			installs, err := installsOf(command)
			if err != nil {
				return err
			}
			provider, id, err := installs.Resolve(args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				id = args[1]
			}

			offer, err := findOffer(command.Context(), sess, provider, id)
			if err != nil {
				return err
			}

			fmt.Printf("%s offers %s (%s)\n  %s\n  %s\n", offer.Name(), offer.Title, offer.ID, offer.Summary, offer.Provider.Hex())
			if m := offer.Manifest; m != nil {
				fmt.Print(iface.Describe(m))
			}
			if yes, _ := command.Flags().GetBool("yes"); !yes {
				fmt.Print("Trust this provider? [y/N] ")
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(answer)) != "y" {
					return errors.New("not installed")
				}
			}
			if offer.Manifest == nil {
				if err := installs.Add(offer, ""); err != nil {
					return err
				}
				fmt.Printf("installed %s: call it with arcn call %s\n", offer.ID, offer.Name())
				return nil
			}
			as, _ := command.Flags().GetString("as")
			if as == "" {
				as = offer.ID
			}
			if builtinName(command.Root(), as) {
				return fmt.Errorf("%s is a command of arcn; choose another name with --as", as)
			}
			if err := installs.Add(offer, as); err != nil {
				return err
			}
			fmt.Printf("installed %s: see arcn help %s\n", as, as)
			return nil
		},
	}
	command.Flags().Bool("yes", false, "trust the provider without asking")
	command.Flags().String("as", "", "the name that runs the capability (default: its id)")
	return command
}

// findOffer reads an offer from the store, and asks the relays when the store
// has none.
func findOffer(ctx context.Context, sess *session, provider nostr.PubKey, id string) (catalog.Offer, error) {
	offer, err := catalog.Find(sess.node.Store, provider, id)
	if err == nil {
		return offer, nil
	}
	reports, errs := sess.node.Pull(ctx, nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}}, sess.relays)
	offer, err = catalog.Find(sess.node.Store, provider, id)
	if unreached := node.Unreached(reports, errs); err != nil && unreached != nil {
		return offer, fmt.Errorf("this machine holds no announcement from that provider, and %w", unreached)
	}
	return offer, err
}

func callCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "call <provider> [body...]",
		Short: "Call an installed capability",
		Long: "With a relay, the call is live: it needs the provider to be present\n" +
			"now, and it prints the reply and the round-trip time. With --later, or\n" +
			"with no relay, the call waits in the outbox and travels like a message;\n" +
			"arcn call results shows the reply once a sync brings it.",
		Args: cobra.MinimumNArgs(1),
		RunE: callCapability,
	}
	command.Flags().Bool("later", false, "store and forward the call, even when a relay is there")
	command.Flags().String("capability", "", "the capability of the provider to call")
	command.Flags().String("method", "", "the method of the call (default: the manifest's)")
	command.Flags().String("path", "", "the path of the call (default: the manifest's)")
	command.Flags().Duration("timeout", 30*time.Second, "how long a live call waits")

	command.AddCommand(&cobra.Command{
		Use: "results", Short: "Show your store-and-forward calls, and their replies", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			found := false
			now := time.Now()
			for _, o := range sess.mail.Outbox() {
				if o.Kind != "request" {
					continue
				}
				found = true
				to, _ := nostr.PubKeyFromHex(o.To)
				fmt.Printf("%s  to %s  %s\n  %s\n", o.Created.Local().Format("2006-01-02 15:04"), identity.Name(to[:]), o.State(now), o.Text)
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
	defer sess.close()

	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	provider, id, err := installs.Resolve(args[0])
	if err != nil {
		return err
	}
	if flag, _ := command.Flags().GetString("capability"); flag != "" {
		id = flag
	}

	offer, err := findOffer(command.Context(), sess, provider, id)
	if err != nil {
		return err
	}
	if !installs.Trusted(provider, offer.ID) {
		return fmt.Errorf("install it first: arcn install %s %s", provider.Hex(), offer.ID)
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
	if path, _ := command.Flags().GetString("path"); path != "" {
		request.Path = path
	}

	later, _ := command.Flags().GetBool("later")
	if later || len(sess.relays) == 0 {
		if _, err := sess.mail.Request(command.Context(), provider, request); err != nil {
			return err
		}
		fmt.Printf("queued for %s: the reply arrives with a sync; see arcn call results\n", offer.Name())
		return nil
	}

	timeout, _ := command.Flags().GetDuration("timeout")
	reply, rtt, via, err := liveCall(command.Context(), sess, provider, request, timeout)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "round trip %s via %s\n", rtt.Round(100*time.Microsecond), via)
	if reply.Err != "" {
		return fmt.Errorf("the provider refused: %s", reply.Err)
	}
	fmt.Print(reply.Body)
	if !strings.HasSuffix(reply.Body, "\n") {
		fmt.Println()
	}
	return nil
}

// publishRelayList tells other citizens which relays this citizen reads its
// mail on, as NIP-17 defines. It also publishes the private relay list of
// NIP-37, which names the relays that hold the citizen's drafts.
func publishRelayList(ctx context.Context, sess *session) {
	var lists []nostr.Event
	list, err := mail.RelayList(sess.signer, sess.urls, nostr.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "the relay list was not signed: %v\n", err)
	} else {
		lists = append(lists, list)
	}
	private, err := draft.RelayList(ctx, sess.keyer, sess.urls, nostr.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "the private relay list was not signed: %v\n", err)
	} else {
		lists = append(lists, private)
	}
	for _, event := range lists {
		_, sent, _ := sess.node.Publish(ctx, event, sess.relays)
		for _, s := range sent {
			if s.Err != nil {
				fmt.Fprintf(os.Stderr, "the relay list did not reach %s: %v\n", s.Transport, s.Err)
			}
		}
	}
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
			defer sess.close()

			announcement, err := catalog.AnnounceManifest(sess.signer, data, nostr.Now())
			if err != nil {
				return err
			}
			if _, sent, err := sess.node.Publish(command.Context(), announcement, sess.relays); err != nil {
				return err
			} else {
				for _, s := range sent {
					if s.Err != nil {
						fmt.Fprintf(os.Stderr, "not sent to %s: %v\n", s.Transport, s.Err)
					}
				}
			}
			fmt.Printf("announced %s as %s\n%s\n", announcement.Tags.GetD(), sess.key.Name(), sess.key.Public.Hex())
			return nil
		},
	}
}
