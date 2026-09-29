package citizen

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/application/catalog"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/transport"
)

type liveAdapter struct{ path }

func (liveAdapter) Watch(context.Context, nostr.Filter) (<-chan nostr.Event, error) {
	ch := make(chan nostr.Event, 1)
	ch <- nostr.Event{Content: "an alternate adapter"}
	close(ch)
	return ch, nil
}

func TestWatchUsesTheLiveInterface(t *testing.T) {
	env := &Environment{Session: &Session{NewRelay: func(string) transport.Transport { return &liveAdapter{} }}}
	ch, err := env.Watch(context.Background(), nostr.Filter{}, []string{"test://live"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := <-ch
	if !ok || got.Err != nil || got.Content != "an alternate adapter" {
		t.Fatalf("watch=%+v open=%v", got, ok)
	}
	for range ch {
	}
}

func TestBareAndAddressTargetsShareResolution(t *testing.T) {
	ctx := context.Background()
	key := keys.Generate()
	s, err := boltstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manifest, err := os.ReadFile("../../cmd/exec-provider/interface.json")
	if err != nil {
		t.Fatal(err)
	}
	event, err := catalog.AnnounceManifest(ctx, key, manifest, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(event); err != nil {
		t.Fatal(err)
	}
	sess := &Session{Node: &node.Node{Store: s}}
	installs := catalog.Installs{Path: filepath.Join(t.TempDir(), "installs.json")}
	npub := nip19.EncodeNpub(key.Public)
	for _, target := range []string{npub, "exec+arc://" + npub + "/"} {
		pk, offer, _, err := sess.Target(ctx, installs, target, "")
		if err != nil || pk != key.Public || offer.ID != "exec" {
			t.Fatalf("target=%s pk=%s offer=%s err=%v", target, pk, offer.ID, err)
		}
	}
}
