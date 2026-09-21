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
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/iface"
	"github.com/spf13/cobra"
)

// dispatch runs a command of an installed capability: arcn <name> <path...>.
// The root command does not parse flags, so the capability reads its own;
// dispatch reads only a leading --home.
func dispatch(command *cobra.Command, args []string) error {
	for len(args) > 0 && strings.HasPrefix(args[0], "--home") {
		value, ok := strings.CutPrefix(args[0], "--home=")
		args = args[1:]
		if !ok {
			if len(args) == 0 {
				return errors.New("--home needs a value")
			}
			value, args = args[0], args[1:]
		}
		if err := command.Flags().Set("home", value); err != nil {
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

	offer, err := findOffer(command.Context(), sess, provider, install.ID)
	if err != nil {
		return err
	}
	if offer.Manifest == nil {
		return fmt.Errorf("%s predates interface version 1: call it with arcn call %s", name, offer.Name())
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
}

func (e *cliEnv) Me() nostr.PubKey { return e.sess.key.Public }

func (e *cliEnv) Name(pk nostr.PubKey) string { return identity.Name(pk[:]) }

func (e *cliEnv) Keyed(info []byte, input string) (string, error) {
	return iface.KeyedValue(e.sess.key.Secret, info, input)
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

// EventAuthor finds the author of an event in the store, then on the relays.
// A coordinate names its author itself.
func (e *cliEnv) EventAuthor(ctx context.Context, ref string) (nostr.PubKey, error) {
	if parts := strings.SplitN(ref, ":", 3); len(parts) == 3 {
		return nostr.PubKeyFromHex(parts[1])
	}
	id, err := nostr.IDFromHex(ref)
	if err != nil {
		return nostr.PubKey{}, err
	}
	filter := nostr.Filter{IDs: []nostr.ID{id}}
	if found := e.sess.node.Store.Query(filter); len(found) > 0 {
		return found[0].PubKey, nil
	}
	e.sess.node.Pull(ctx, filter, e.sess.relays)
	if found := e.sess.node.Store.Query(filter); len(found) > 0 {
		return found[0].PubKey, nil
	}
	return nostr.PubKey{}, fmt.Errorf("no store and no relay holds the event %s", ref)
}

func (e *cliEnv) Keyer() nostr.Keyer { return e.sess.keyer }

// Publish keeps each event, then sends it to every relay. An event that a
// relay did not take stays in the store, and the next sync sends it.
func (e *cliEnv) Publish(ctx context.Context, events []nostr.Event) error {
	unsent := map[string]error{}
	for _, event := range events {
		result, sent, err := e.sess.node.Publish(ctx, event, e.sess.relays)
		if err != nil {
			return err
		}
		if result.Outcome == store.Refused {
			return fmt.Errorf("the store refused the event: %s", result.Reason)
		}
		for _, s := range sent {
			if s.Err != nil && unsent[s.Transport] == nil {
				unsent[s.Transport] = s.Err
			}
		}
	}
	for name, err := range unsent {
		fmt.Fprintf(os.Stderr, "not sent to %s: %v\nit waits in the store; arcn sync sends it\n", name, err)
	}
	return nil
}

// Fetch asks the relays for what matches, keeps it, and reads the store. A
// filter of IDs asks only for the events that the store lacks.
func (e *cliEnv) Fetch(ctx context.Context, filter nostr.Filter) ([]nostr.Event, error) {
	if filter.IDs != nil {
		// An empty list of IDs matches nothing. A relay would read it as no
		// condition, and send everything.
		if len(filter.IDs) == 0 {
			return nil, nil
		}
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
	if _, errs := e.sess.node.Pull(ctx, filter, e.sess.relays); len(errs) > 0 && len(errs) == len(e.sess.relays) {
		fmt.Fprintf(os.Stderr, "no relay answered, so this shows what this machine holds: %v\n", errs[0])
	}
	return e.sess.node.Store.Query(filter), nil
}

// Watch passes on new events from every relay, until all of them end.
func (e *cliEnv) Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error) {
	if len(e.sess.relays) == 0 {
		return nil, errors.New("no relay to watch: add one with arcn relay add")
	}
	out := make(chan nostr.Event)
	var wg sync.WaitGroup
	for _, t := range e.sess.relays {
		in, err := e.sess.node.Watch(ctx, filter, t.(relay.Relay))
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
		reply, rtt, err := call.Live(ctx, sess.key, provider, request, t.(relay.Relay))
		cancel()
		if err == nil {
			return reply, rtt, t.Name(), nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v", t.Name(), err))
	}
	return call.Reply{}, 0, "", fmt.Errorf("no live path to %s:\n  %s\nuse --later to store and forward the call",
		identity.Name(provider[:]), strings.Join(failures, "\n  "))
}
