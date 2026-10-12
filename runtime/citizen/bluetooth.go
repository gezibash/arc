package citizen

import (
	"context"
	"errors"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/ble"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/catalog"
)

// BluetoothConfig selects an exclusive direct path. The peer key is required;
// an address selects dialing, and Listen selects successive incoming links.
type BluetoothConfig struct {
	Peer             nostr.PubKey
	Address, Adapter string
	Listen           bool
}

func (s *Session) useBluetooth(ctx context.Context, c BluetoothConfig) error {
	if c.Peer == (nostr.PubKey{}) {
		return errors.New("bluetooth: expected peer key is required")
	}
	if s.Remote {
		return errors.New("bluetooth: use a local signing key")
	}
	if c.Listen == (c.Address != "") {
		return errors.New("bluetooth: choose either a peer address or listen")
	}
	cfg := ble.DirectConfig{Peer: c.Peer, Address: c.Address, Adapter: c.Adapter, Signer: s.Signer, Store: s.store}
	// Only our service announcements and sealed mail addressed to this peer
	// are available to remote queries. Journals, drafts and other mail stay local.
	cfg.Share = func(e nostr.Event) bool {
		return (e.Kind == catalog.Kind && e.PubKey == s.Signer.PublicKey()) ||
			(e.Kind == private.WrapKind && (nostr.Filter{Tags: nostr.TagMap{"p": []string{c.Peer.Hex()}}}).Matches(e))
	}
	var t interface {
		transport.Live
		Close() error
	}
	var err error
	if c.Listen {
		t, err = ble.Listen(ctx, cfg)
	} else {
		t, err = ble.OpenDirect(ctx, cfg)
	}
	if err != nil {
		return err
	}
	s.bluetooth = t
	s.Bluetooth = &c
	s.Relays = []transport.Transport{t}
	s.Indexers = nil
	s.URLs = nil
	s.NewRelay = func(string) transport.Transport { return disabledRelay{} }
	return nil
}

type disabledRelay struct{}

func (disabledRelay) Name() string { return "disabled-in-bluetooth-mode" }
func (disabledRelay) Send(context.Context, nostr.Event) error {
	return errors.New("bluetooth: internet relay routing is disabled")
}
func (disabledRelay) Fetch(context.Context, nostr.Filter) (transport.Batch, error) {
	return transport.Batch{}, errors.New("bluetooth: internet relay routing is disabled")
}
