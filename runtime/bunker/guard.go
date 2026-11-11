package bunker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/terminal"
)

// Guard decides which devices a bunker admits. With no device list, it
// admits a device that gave the bunker secret once, as before device lists.
// With a list, it admits only the device keys on the list. Make one with
// NewGuard: the zero Guard has no secret, and is not usable.
type Guard struct {
	mu         sync.Mutex
	secret     string
	authorized map[nostr.PubKey]bool
	list       *devices.List
}

// NewGuard admits, until a device list comes, each device that gives the
// secret of the bunker once.
func NewGuard(secret string) *Guard {
	return &Guard{secret: secret, authorized: map[nostr.PubKey]bool{}}
}

func (g *Guard) Authorize(from nostr.PubKey, given string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.list != nil {
		return g.list.Has(from)
	}
	if given == g.secret {
		g.authorized[from] = true
	}
	return g.authorized[from]
}

// Refusal says why the bunker refuses a device, or nothing.
func (g *Guard) Refusal(from nostr.PubKey) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.list != nil && !g.list.Has(from) {
		return errors.New("the signer refuses this device: its key is not on the device list")
	}
	return nil
}

// load takes a list when it is newer than the list that the guard holds.
func (g *Guard) load(l devices.List) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.list != nil && l.At <= g.list.At {
		return false
	}
	g.list = &l
	return true
}

func (g *Guard) Listed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.list != nil
}

// WatchDevices loads each new device list that reaches the bunker's relay,
// until ctx ends. The relay serves the sealed list only after the bunker
// authenticates as the citizen.
func WatchDevices(ctx context.Context, r relay.Relay, k keys.Signer, g *Guard, log *slog.Logger) {
	for {
		events, err := r.Watch(ctx, devices.Filter(k.PublicKey()))
		if err == nil {
			for event := range events {
				l, err := devices.Open(ctx, k, k.PublicKey(), event)
				if err != nil {
					log.Warn("a device list does not open", "error", err)
					continue
				}
				if g.load(l) {
					log.Info("loaded the device list", "devices", len(l.Devices))
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("the relay ended the watch of the device list; watching again", "relay", r.URL, "error", err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// startDevices loads the newest device list that the store and the relays
// hold, before the bunker answers anyone.
func startDevices(ctx context.Context, sess *citizen.Session, r relay.Relay, g *Guard, errs io.Writer) error {
	filter := devices.Filter(sess.Key.Public)
	sess.Node.Pull(ctx, filter, sess.Relays)
	held, err := sess.Node.Store.Query(filter)
	if err != nil {
		return err
	}
	if batch, err := r.Fetch(ctx, filter); err == nil {
		held = append(held, batch.Events...)
	} else {
		fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("the device list did not come from %s: %v", r.URL, err)))
	}
	if l, ok, err := citizen.NewestDeviceList(ctx, sess.Signer, sess.Key.Public, held); err != nil {
		return err
	} else if ok {
		g.load(l)
	}
	return nil
}
