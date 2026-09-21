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
	relay  *relays.Selection
}

// open reads the flags of a command, and finds the identity and the relay.
func open(command *cobra.Command, needRelay bool) (*settings, error) {
	dir, _ := command.Flags().GetString("store")
	name, _ := command.Flags().GetString("key")

	keys, err := keyStore(dir)
	if err != nil {
		return nil, err
	}

	me, err := activeIdentity(keys, name)
	if err != nil {
		return nil, err
	}

	held := &settings{keys: keys, relays: &relays.Store{Dir: keys.Dir}, me: me}
	if !needRelay {
		return held, nil
	}

	address, _ := command.Flags().GetString("relay")
	pin, _ := command.Flags().GetString("relay-pubkey")

	if held.relay, err = held.relays.Resolve(address, pin); err != nil {
		return nil, err
	}
	if len(held.relay.Pin) == 0 {
		return nil, fmt.Errorf("no public key is pinned for %s: join it again with arc join", held.relay.Address)
	}
	return held, nil
}

// dial joins the relay as the active identity. The wake hooks of the store
// wake a citizen that pauses before each request to it.
func (s *settings) dial(ctx context.Context) (*client.Client, error) {
	waker, err := wake.Load(filepath.Join(s.keys.Dir, wake.FileName), filepath.Join(s.keys.Dir, wake.StateDirName))
	if err != nil {
		return nil, err
	}

	return client.Dial(ctx, s.relay.Address, client.Options{
		Identity:       s.me,
		RelayPublicKey: s.relay.Pin,
		Waker:          waker,
	})
}

func keyStore(dir string) (*identity.Store, error) {
	if dir != "" {
		return &identity.Store{Dir: dir}, nil
	}
	return identity.DefaultStore()
}

// activeIdentity picks the identity: the --key flag, then the rules of the
// key store.
func activeIdentity(keys *identity.Store, name string) (*identity.Identity, error) {
	if name != "" {
		return keys.Get(name)
	}

	me, _, err := keys.Active()
	if errors.Is(err, identity.ErrNoDefault) || errors.Is(err, identity.ErrNotFound) {
		return nil, errors.New("no identity: make one with arc keys gen")
	}
	return me, err
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
