package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip05"
	"fiatjaf.com/nostr/nip19"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/iface"
	"github.com/spf13/cobra"
)

// dispatch runs a command of an installed capability: arcn <name> <path...>.
// The root command does not parse flags, so the capability reads its own;
// dispatch reads only a leading --home and --key.
func dispatch(command *cobra.Command, args []string) error {
	for len(args) > 0 {
		flag := ""
		for _, name := range []string{"home", "key"} {
			if args[0] == "--"+name || strings.HasPrefix(args[0], "--"+name+"=") {
				flag = name
			}
		}
		if flag == "" {
			break
		}
		value, ok := strings.CutPrefix(args[0], "--"+flag+"=")
		args = args[1:]
		if !ok {
			if len(args) == 0 {
				return fmt.Errorf("--%s needs a value", flag)
			}
			value, args = args[0], args[1:]
		}
		if err := command.Flags().Set(flag, value); err != nil {
			return err
		}
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return command.Help()
	}
	return runCapability(command, args[0], args[1:])
}

func runCapability(command *cobra.Command, name string, words []string) error {
	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	install, ok := installs.Named(name)
	if !ok {
		return fmt.Errorf("unknown command %q: see arcn --help, or install a capability with arcn install", name)
	}
	provider, err := nostr.PubKeyFromHex(install.Provider)
	if err != nil {
		return err
	}

	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.close()

	// Ask for the newest announcement first, so a new version meets the
	// consent check at once.
	if err := node.Unreached(sess.node.Pull(command.Context(), nostr.Filter{
		Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}, Tags: nostr.TagMap{"d": {install.ID}},
	}, sess.relays)); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nthis uses the announcement that this machine holds\n", err)
	}
	offer, err := findOffer(command.Context(), sess, provider, install.ID)
	if err != nil {
		return err
	}
	if offer.Manifest == nil {
		return fmt.Errorf("%s predates interface version 1: call it with arcn call %s", name, offer.Name())
	}
	// A new version of the manifest can do no more than the citizen agreed
	// to, until they agree again.
	var changes []string
	if install.Consent == nil {
		changes = []string{"this install records no consent"}
	} else {
		changes = install.Consent.Changes(offer.Manifest)
	}
	if len(changes) > 0 {
		return fmt.Errorf("the author changed what %s can do:\n  %s\nto agree, install it again: arcn install %s %s --as %s",
			name, strings.Join(changes, "\n  "), install.Provider, install.ID, name)
	}

	env := &cliEnv{sess: sess, installs: installs}
	in := iface.Installed{Manifest: offer.Manifest, Author: provider, Name: name}
	return iface.Run(command.Context(), env, in, words, iface.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
}

// helpCommand shows the help of a command of arcn, or of a capability.
func helpCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command, or an installed capability",
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 0 {
				return root.Help()
			}
			if found, _, err := root.Find(args); err == nil && found != root {
				return found.Help()
			}
			return runCapability(command, args[0], append(args[1:], "help"))
		},
	}
}

// builtinName says whether a name is a command of arcn itself.
func builtinName(root *cobra.Command, name string) bool {
	for _, c := range root.Commands() {
		if c.Name() == name || slices.Contains(c.Aliases, name) {
			return true
		}
	}
	return name == "help" || name == "completion"
}

// cliEnv is what a capability needs from this machine.
type cliEnv struct {
	sess     *session
	installs catalog.Installs
	root     []byte
}

func (e *cliEnv) Me() nostr.PubKey { return e.sess.key.Public }

func (e *cliEnv) Name(pk nostr.PubKey) string { return identity.Name(pk[:]) }

func (e *cliEnv) Keyed(info []byte, input string) (string, error) {
	if e.root == nil {
		// The root draft appears only once the citizen uses keyed values,
		// so a citizen who never does leaves no signed event behind.
		if !e.sess.remote {
			publishKeyedRoot(context.Background(), e.sess)
		}
		root, err := keyedRoot(context.Background(), e.sess)
		if err != nil {
			return "", err
		}
		e.root = root
	}
	return iface.KeyedValue(e.root, info, input)
}

// ResolveKey reads 64 hex characters, an npub, an nprofile, a NIP-05 name,
// or the name of an install.
func (e *cliEnv) ResolveKey(ctx context.Context, text string) (nostr.PubKey, error) {
	if pk, err := nostr.PubKeyFromHex(text); err == nil {
		return pk, nil
	}
	if prefix, value, err := nip19.Decode(text); err == nil {
		switch prefix {
		case "npub":
			return value.(nostr.PubKey), nil
		case "nprofile":
			return value.(nostr.ProfilePointer).PublicKey, nil
		}
	}
	list, _ := e.installs.List()
	for _, install := range list {
		if install.As == text || install.Name == text {
			return nostr.PubKeyFromHex(install.Provider)
		}
	}
	if strings.Contains(text, ".") && nip05.IsValidIdentifier(text) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		pointer, err := nip05.QueryIdentifier(ctx, text)
		if err != nil {
			return nostr.PubKey{}, fmt.Errorf("the NIP-05 name %s did not resolve: %w", text, err)
		}
		return pointer.PublicKey, nil
	}
	return nostr.PubKey{}, fmt.Errorf("%q is not a key: use hex, an npub, an nprofile, a NIP-05 name, or an installed name", text)
}

func (e *cliEnv) Keyer() nostr.Keyer { return e.sess.keyer }

// relaysOf makes transports of relay URLs. No URLs means the citizen's own
// relays.
func (e *cliEnv) relaysOf(urls []string) []transport.Transport {
	if urls == nil {
		return e.sess.relays
	}
	out := make([]transport.Transport, 0, len(urls))
	for _, url := range urls {
		out = append(out, relay.Relay{URL: url, Signer: e.sess.keyer})
	}
	return out
}

// Publish keeps each event, then sends it. An event that one of the
// citizen's relays did not take stays in the store, and the next sync sends
// it. An event for named relays, such as a group's, fails when none of them
// takes it: no other path reaches a group.
func (e *cliEnv) Publish(ctx context.Context, events []nostr.Event, urls []string) error {
	targets := e.relaysOf(urls)
	unsent := map[string]error{}
	for _, event := range events {
		result, sent, err := e.sess.node.Publish(ctx, event, targets)
		if err != nil {
			return err
		}
		if result.Outcome == store.Refused {
			return fmt.Errorf("the store refused the event: %s", result.Reason)
		}
		taken := false
		for _, s := range sent {
			if s.Err == nil {
				taken = true
			} else if unsent[s.Transport] == nil {
				unsent[s.Transport] = s.Err
			}
		}
		if urls != nil && !taken {
			return fmt.Errorf("the relay did not take the event: %v", sent[0].Err)
		}
	}
	if urls == nil {
		for name, err := range unsent {
			fmt.Fprintf(os.Stderr, "not sent to %s: %v\nit waits in the store; arcn sync sends it\n", name, err)
		}
	}
	return nil
}

// Fetch reads events. With no URLs, it asks the citizen's relays, keeps what
// they send, and reads the store; a filter of IDs asks only for the events
// that the store lacks. With URLs, it returns what those relays hold now.
func (e *cliEnv) Fetch(ctx context.Context, filter nostr.Filter, urls []string) ([]nostr.Event, error) {
	// An empty list of IDs matches nothing. A relay would read it as no
	// condition, and send everything.
	if filter.IDs != nil && len(filter.IDs) == 0 {
		return nil, nil
	}
	if urls != nil {
		var out []nostr.Event
		var failures []string
		for _, t := range e.relaysOf(urls) {
			batch, err := t.Fetch(ctx, filter)
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			out = append(out, batch.Events...)
		}
		if len(failures) == len(urls) {
			return nil, fmt.Errorf("no relay answered: %s", strings.Join(failures, "; "))
		}
		slices.SortFunc(out, func(a, b nostr.Event) int { return int(b.CreatedAt) - int(a.CreatedAt) })
		return out, nil
	}
	if filter.IDs != nil && filter.Kinds == nil {
		found, err := e.sess.node.Obtain(ctx, filter.IDs, e.sess.relays)
		if err != nil {
			return nil, err
		}
		out := make([]nostr.Event, 0, len(found))
		for _, event := range found {
			out = append(out, event)
		}
		return out, nil
	}
	if filter.IDs != nil {
		// Ask the relays only for what the store lacks.
		var missing []nostr.ID
		for _, id := range filter.IDs {
			if !e.sess.node.Store.Has(id) {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			return e.sess.node.Store.Query(filter), nil
		}
		ask := filter
		ask.IDs = missing
		reports, errs := e.sess.node.Pull(ctx, ask, e.sess.relays)
		found := e.sess.node.Store.Query(filter)
		// An event that is still missing because no relay answered is an
		// error of the relays, not of the event.
		if err := node.Unreached(reports, errs); err != nil && len(found) < len(filter.IDs) {
			return found, err
		}
		return found, nil
	}
	if err := node.Unreached(e.sess.node.Pull(ctx, filter, e.sess.relays)); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nthis shows what this machine holds\n", err)
	}
	return e.sess.node.Store.Query(filter), nil
}

// Watch passes on new events from every relay, until all of them end.
func (e *cliEnv) Watch(ctx context.Context, filter nostr.Filter, urls []string) (<-chan nostr.Event, error) {
	targets := e.relaysOf(urls)
	if len(targets) == 0 {
		return nil, errors.New("no relay to watch: add one with arcn relay add")
	}
	out := make(chan nostr.Event)
	var wg sync.WaitGroup
	for _, t := range targets {
		var in <-chan nostr.Event
		var err error
		if urls == nil {
			in, err = e.sess.node.Watch(ctx, filter, t.(relay.Relay))
		} else {
			in, err = t.(relay.Relay).Watch(ctx, filter)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", t.Name(), err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for event := range in {
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(out) }()
	return out, nil
}

// SendPrivate seals a private event through the mail layer.
func (e *cliEnv) SendPrivate(ctx context.Context, to nostr.PubKey, kind nostr.Kind, content string, tags nostr.Tags) error {
	_, err := e.sess.mail.SendRumor(ctx, to, kind, content, tags)
	return err
}

// Private syncs the mail with every relay, and reads the private events.
func (e *cliEnv) Private(ctx context.Context, kinds []nostr.Kind) ([]nostr.Event, error) {
	for _, t := range e.sess.relays {
		report, err := e.sess.mail.Sync(ctx, t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: mail: %v\n", t.Name(), err)
			continue
		}
		if n := len(report.Refused); n > 0 {
			fmt.Fprintf(os.Stderr, "%s: %d messages did not open: %s\n", t.Name(), n, report.Refused[0])
		}
	}
	return e.sess.mail.Rumors(kinds), nil
}

// Call sends one request: live over a relay, or later through the outbox.
func (e *cliEnv) Call(ctx context.Context, provider nostr.PubKey, request iface.CallRequest, later bool) (iface.CallResult, error) {
	r := call.Request{Capability: request.Capability, Method: request.Method, Path: request.Path, Body: request.Body}
	if later {
		if _, err := e.sess.mail.Request(ctx, provider, r); err != nil {
			return iface.CallResult{}, err
		}
		return iface.CallResult{Queued: true}, nil
	}
	reply, _, _, err := liveCall(ctx, e.sess, provider, r, 30*time.Second)
	if err != nil {
		return iface.CallResult{}, err
	}
	return iface.CallResult{Body: reply.Body, Err: reply.Err}, nil
}

// liveCall tries each relay in turn, and returns the first reply, the round
// trip, and the relay that carried it.
func liveCall(ctx context.Context, sess *session, provider nostr.PubKey, request call.Request, timeout time.Duration) (call.Reply, time.Duration, string, error) {
	if len(sess.relays) == 0 {
		return call.Reply{}, 0, "", errors.New("no relay, so no live path: add a relay, or use --later to store and forward the call")
	}
	var failures []string
	for _, t := range sess.relays {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		reply, rtt, err := call.Live(ctx, sess.signer, provider, request, t.(relay.Relay))
		cancel()
		if err == nil {
			return reply, rtt, t.Name(), nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v", t.Name(), err))
	}
	return call.Reply{}, 0, "", fmt.Errorf("no live path to %s:\n  %s\nuse --later to store and forward the call",
		identity.Name(provider[:]), strings.Join(failures, "\n  "))
}
