package citizen

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/application/catalog"
	"github.com/gezibash/arc/application/iface"
	"github.com/gezibash/arc/application/lists"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// Environment is what a capability needs from this machine.
type Environment struct {
	Session  *Session
	Installs catalog.Installs
	root     []byte
	// tool is the installed name of the running command, and lists holds
	// its lists.
	Tool  string
	Lists *lists.Store
}

func (e *Environment) Me() nostr.PubKey { return e.Session.Key.Public }

func (e *Environment) Name(pk nostr.PubKey) string { return keys.Name(pk[:]) }

func (e *Environment) Keyed(ctx context.Context, info []byte, input string) (string, error) {
	if e.root == nil {
		// The root draft appears only once the citizen uses keyed values,
		// so a citizen who never does leaves no signed event behind.
		if !e.Session.Remote {
			if err := e.Session.PublishKeyedRoot(ctx); err != nil {
				return "", err
			}
		}
		root, err := e.Session.KeyedRoot(ctx)
		if err != nil {
			return "", err
		}
		e.root = root
	}
	return iface.KeyedValue(e.root, info, input)
}

// ResolveKey reads 64 hex characters, an npub, an nprofile, a NIP-05 name,
// or the name of an install.
func (e *Environment) ResolveKey(ctx context.Context, text string) (nostr.PubKey, error) {
	pk, _, err := e.Installs.Resolve(ctx, text)
	return pk, err
}

func (e *Environment) Keyer() nostr.Keyer { return e.Session.Signer }

// relaysOf makes transports of relay URLs. No URLs means the citizen's own
// relays.
func (e *Environment) relaysOf(urls []string) []transport.Transport {
	if urls == nil {
		return e.Session.Relays
	}
	out := make([]transport.Transport, 0, len(urls))
	for _, url := range urls {
		out = append(out, e.Session.NewRelay(url))
	}
	return out
}

// Publish keeps each event, then sends it. An event that one of the
// citizen's relays did not take stays in the store, and the next sync sends
// it. An event for named relays, such as a group's, fails when none of them
// takes it: no other path reaches a group.
func (e *Environment) Publish(ctx context.Context, events []nostr.Event, urls []string) error {
	targets := e.relaysOf(urls)
	unsent := map[string]error{}
	for _, event := range events {
		result, sent, err := e.Session.Node.Publish(ctx, event, targets)
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
			if len(sent) == 0 {
				return errors.New("no relay to publish to")
			}
			return fmt.Errorf("the relay did not take the event: %v", sent[0].Err)
		}
	}
	if urls == nil {
		for name, err := range unsent {
			e.Session.warnf("not sent to %s: %v\nit waits in the store; arc sync sends it\n", name, err)
		}
	}
	return nil
}

// Fetch reads events. With no URLs, it asks the citizen's relays, keeps what
// they send, and reads the store; a filter of IDs asks only for the events
// that the store lacks. With URLs, it returns what those relays hold now.
func (e *Environment) Fetch(ctx context.Context, filter nostr.Filter, urls []string) ([]nostr.Event, error) {
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
		found, err := e.Session.Node.Obtain(ctx, filter.IDs, e.Session.Relays)
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
			held, err := e.Session.Node.Store.Has(id)
			if err != nil {
				return nil, err
			}
			if !held {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			return e.Session.Node.Store.Query(filter)
		}
		ask := filter
		ask.IDs = missing
		reports, errs := e.Session.Node.Pull(ctx, ask, e.Session.Relays)
		found, err := e.Session.Node.Store.Query(filter)
		if err != nil {
			return nil, err
		}
		// An event that is still missing because no relay answered is an
		// error of the relays, not of the event.
		if err := node.Unreached(reports, errs); err != nil && len(found) < len(filter.IDs) {
			return found, err
		}
		return found, nil
	}
	if err := node.Unreached(e.Session.Node.Pull(ctx, filter, e.Session.Relays)); err != nil {
		e.Session.warnf("%v\nthis shows what this machine holds\n", err)
	}
	return e.Session.Node.Store.Query(filter)
}

// Watch passes on new events from every relay, until all of them end.
func (e *Environment) Watch(ctx context.Context, filter nostr.Filter, urls []string) (<-chan transport.Received, error) {
	targets := e.relaysOf(urls)
	if len(targets) == 0 {
		return nil, errors.New("no relay to watch: add one with arc relay add")
	}
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan transport.Received)
	var wg sync.WaitGroup
	var failures []error
	started := 0
	for _, t := range targets {
		live, ok := t.(transport.Live)
		if !ok {
			failures = append(failures, fmt.Errorf("%s does not support live watches", t.Name()))
			continue
		}
		var stored <-chan transport.Received
		var raw <-chan nostr.Event
		var err error
		if urls == nil {
			stored, err = e.Session.Node.Watch(ctx, filter, live)
		} else {
			raw, err = live.Watch(ctx, filter)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", t.Name(), err))
			continue
		}
		started++
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var received transport.Received
				select {
				case event, ok := <-raw:
					if !ok {
						return
					}
					received.Event = event
				case event, ok := <-stored:
					if !ok {
						return
					}
					received = event
				case <-ctx.Done():
					return
				}
				select {
				case out <- received:
				case <-ctx.Done():
					return
				}
				if received.Err != nil {
					cancel()
					return
				}
			}
		}()
	}
	if started == 0 {
		cancel()
		return nil, errors.Join(failures...)
	}
	for _, err := range failures {
		e.Session.warnf("%v\n", err)
	}
	go func() { wg.Wait(); cancel(); close(out) }()
	return out, nil
}

// SendPrivate seals a private event through the mail layer.
func (e *Environment) SendPrivate(ctx context.Context, to nostr.PubKey, kind nostr.Kind, content string, tags nostr.Tags) error {
	_, err := e.Session.Mail.SendRumor(ctx, to, kind, content, tags)
	return err
}

// Private syncs the mail with every relay, and reads the private events.
func (e *Environment) Private(ctx context.Context, kinds []nostr.Kind) ([]nostr.Event, error) {
	for _, t := range e.Session.Relays {
		report, err := e.Session.Mail.Sync(ctx, t)
		if err != nil {
			e.Session.warnf("%s: mail: %v\n", t.Name(), err)
			continue
		}
		if n := len(report.Refused); n > 0 {
			e.Session.warnf("%s: %d messages did not open: %s\n", t.Name(), n, report.Refused[0])
		}
	}
	return e.Session.Mail.Rumors(ctx, kinds)
}

// Call sends one request: live over a relay, or later through the outbox.
func (e *Environment) Call(ctx context.Context, provider nostr.PubKey, request iface.CallRequest, later bool) (iface.CallResult, error) {
	r := call.Request{Capability: request.Capability, Method: request.Method, Path: request.Path, Body: request.Body}
	if later {
		if _, err := e.Session.Mail.Request(ctx, provider, r); err != nil {
			return iface.CallResult{}, err
		}
		return iface.CallResult{Queued: true}, nil
	}
	reply, _, _, err := e.Session.LiveCall(ctx, provider, r, call.Timeout)
	if err != nil {
		return iface.CallResult{}, err
	}
	return iface.CallResult{Body: reply.Body, Err: reply.Err}, nil
}

// List returns the members of a list of the running command, as keys.
func (e *Environment) List(name string) ([]nostr.PubKey, error) {
	if e.Lists == nil {
		return nil, nil
	}
	var out []nostr.PubKey
	members, err := e.Lists.Members(e.Tool, name)
	if err != nil {
		return nil, err
	}
	for _, member := range members {
		if pk, err := nostr.PubKeyFromHex(member); err == nil {
			out = append(out, pk)
		}
	}
	return out, nil
}
