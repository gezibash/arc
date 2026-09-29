package catalog_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"fiatjaf.com/nostr/nip19"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/keys"
)

func TestResolvePublicFormsAndReportBrokenInstalls(t *testing.T) {
	key := keys.Generate()
	installs := catalog.Installs{Path: filepath.Join(t.TempDir(), "installs.json")}
	if err := installs.Add(catalog.Offer{Provider: key.Public, ID: "counter"}, "count"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{key.Public.Hex(), nip19.EncodeNpub(key.Public), nip19.EncodeNprofile(key.Public, nil), "count"} {
		pk, _, err := installs.Resolve(context.Background(), name)
		if err != nil || pk != key.Public {
			t.Errorf("resolve %s: %s %v", name, pk, err)
		}
	}
	if err := os.WriteFile(installs.Path, []byte("broken JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installs.Resolve(context.Background(), "count"); err == nil {
		t.Fatal("broken installs appeared to contain no alias")
	}
	if _, err := installs.Trusted(key.Public, "counter"); err == nil {
		t.Fatal("broken installs appeared untrusted")
	}
}

func TestConcurrentInstallsAreAllRetained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installs.json")
	var group sync.WaitGroup
	start := make(chan struct{})
	for n := range 24 {
		offer := catalog.Offer{Provider: keys.Generate().Public, ID: "counter"}
		group.Go(func() {
			<-start
			if err := (catalog.Installs{Path: path}).Add(offer, fmt.Sprintf("count-%02d", n)); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	group.Wait()
	all, err := (catalog.Installs{Path: path}).List()
	if err != nil || len(all) != 24 {
		t.Fatalf("installs=%d err=%v", len(all), err)
	}
}
