package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/relays"
	"github.com/gezibash/arc/wake"
	"github.com/spf13/cobra"
)

// settings holds what every command needs: the identity, and the relay.
type settings struct {
	keys   *identity.Store
	relays *relays.Store
	me     *identity.Identity
	source identity.Source
	relay  *relays.Selection
}

// fromFlag is the source of an identity that --key names.
const fromFlag identity.Source = "--key"

// open reads the flags of a command, and finds the identity and the relay.
func open(command *cobra.Command, needRelay bool) (*settings, error) {
	dir, _ := command.Flags().GetString("store")
	name, _ := command.Flags().GetString("key")

	keys, err := keyStore(dir)
	if err != nil {
		return nil, err
	}

	me, source, err := activeIdentity(keys, name)
	if err != nil {
		return nil, err
	}

	held := &settings{keys: keys, relays: &relays.Store{Dir: keys.Dir}, me: me, source: source}
	if !needRelay {
		return held, nil
	}
	if held.relay, err = selectRelay(command, held.relays); err != nil {
		return nil, err
	}
	return held, nil
}

// openAnonymous finds the relay, and holds a temporary identity that exists
// only in memory. The relay keeps one route for each identity, so a command
// that uses this identity never replaces the route of a running citizen.
func openAnonymous(command *cobra.Command) (*settings, error) {
	dir, _ := command.Flags().GetString("store")
	keys, err := keyStore(dir)
	if err != nil {
		return nil, err
	}

	me, err := identity.Generate()
	if err != nil {
		return nil, err
	}

	held := &settings{keys: keys, relays: &relays.Store{Dir: keys.Dir}, me: me}
	if held.relay, err = selectRelay(command, held.relays); err != nil {
		return nil, err
	}
	return held, nil
}

// selectRelay reads the relay: the flags, then the environment, then the
// relay that arc join saved. A relay needs a pinned key.
func selectRelay(command *cobra.Command, store *relays.Store) (*relays.Selection, error) {
	address, _ := command.Flags().GetString("relay")
	pin, _ := command.Flags().GetString("relay-pubkey")

	selected, err := store.Resolve(address, pin)
	if err != nil {
		return nil, err
	}
	if len(selected.Pin) == 0 {
		return nil, fmt.Errorf("no public key is pinned for %s: join it again with arc join", selected.Address)
	}
	return selected, nil
}

// dial joins the relay as the active identity. Before each request, the
// wake flow of the store wakes a citizen that pauses, and refuses a citizen
// that is not there.
func (s *settings) dial(ctx context.Context) (*client.Client, error) {
	return client.Dial(ctx, s.relay.Address, client.Options{
		Identity:       s.me,
		RelayPublicKey: s.relay.Pin,
		Waker:          s.waker(),
	})
}

// waker reads the wake hooks of the store.
func (s *settings) waker() *wake.Waker {
	return wake.Load(filepath.Join(s.keys.Dir, wake.FileName), filepath.Join(s.keys.Dir, wake.StateDirName))
}

func keyStore(dir string) (*identity.Store, error) {
	if dir != "" {
		return &identity.Store{Dir: dir}, nil
	}
	return identity.DefaultStore()
}

// activeIdentity picks the identity: the --key flag, then the rules of the
// key store. It says which of them chose it.
func activeIdentity(keys *identity.Store, name string) (*identity.Identity, identity.Source, error) {
	if name != "" {
		me, err := keys.Select(fromFlag, name)
		return me, fromFlag, err
	}

	me, source, err := keys.Active()
	if errors.Is(err, identity.ErrNoDefault) {
		return nil, source, errors.New("no identity: make one with arc keys gen")
	}
	return me, source, err
}

// deadline gives a command a bounded run.
func deadline(seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func stderrLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
