// Command arc-relay runs one relay.
//
//	arc-relay --address :7331 --key my-relay
//
// The relay routes encrypted packets by public key, and holds a small
// directory of signed announcements. It never holds a key of a citizen.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/relay"
)

// version is the release of this relay. A build sets it with
// -ldflags "-X main.version=0.6.0".
var version = "dev"

func main() {
	address := flag.String("address", ":7331", "where the relay listens")
	key := flag.String("key", "", "the petname of the relay identity, or a prefix of it")
	store := flag.String("store", "", "the directory of ARC (default ~/.config/arc)")
	maxFrame := flag.Uint("max-frame-bytes", 0, "the largest frame that the relay accepts, or 0 for any size")
	generate := flag.Bool("generate", false, "make the identity when the store does not hold it")
	transit := flag.Bool("transit", false, "let the traffic of partner relays pass through this one")

	var peers peerList
	flag.Var(&peers, "peer", "a partner relay, as <public key hex>@host:port. Repeat for more.")
	flag.Parse()

	if err := run(*address, *key, *store, uint32(*maxFrame), *generate, *transit, peers); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// peerList reads one --peer flag for each partner.
type peerList []relay.Peer

func (p *peerList) String() string { return "" }

// Set reads "<public key hex>@host:port".
func (p *peerList) Set(value string) error {
	key, address, found := strings.Cut(value, "@")
	if !found || address == "" {
		return errors.New("a peer reads as <public key hex>@host:port")
	}

	raw, err := hex.DecodeString(strings.ToLower(key))
	if err != nil || len(raw) != 32 {
		return errors.New("a peer key holds 64 characters of hex")
	}

	*p = append(*p, relay.Peer{PublicKey: raw, Address: address})
	return nil
}

func run(address, key, storeDir string, maxFrame uint32, generate, transit bool, peers []relay.Peer) error {
	keys, err := openStore(storeDir)
	if err != nil {
		return err
	}

	me, err := relayIdentity(keys, key, generate)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := relay.Listen(ctx, relay.Options{
		Identity:      me,
		Address:       address,
		MaxFrameBytes: maxFrame,
		Version:       version,
		Peers:         peers,
		Transit:       transit,
		Log:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		return err
	}

	fmt.Printf("relay %s %s on %s\n", me.Name(), me.EncodePublicKey(), server.Addr().String())
	os.Stdout.Sync()

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "the relay is stopping")
	return server.Close()
}

func openStore(dir string) (*identity.Store, error) {
	if dir != "" {
		return &identity.Store{Dir: dir}, nil
	}
	return identity.DefaultStore()
}

// relayIdentity picks the key of the relay: the one that --key names, or the
// active identity of this shell.
func relayIdentity(keys *identity.Store, name string, generate bool) (*identity.Identity, error) {
	if name == "" {
		me, _, err := keys.Active()
		if err != nil {
			return nil, fmt.Errorf("no identity: name one with --key, or make one with arc keys gen: %w", err)
		}
		return me, nil
	}

	me, err := keys.Get(name)
	if err == nil {
		return me, nil
	}
	if !generate {
		return nil, fmt.Errorf("the store holds no key for %q: %w", name, err)
	}
	return keys.Generate()
}
