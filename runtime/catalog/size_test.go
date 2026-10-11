package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/runtime/capability"
	"github.com/gezibash/arc/runtime/catalog"
)

// withSummary is the manifest of the exec app with a summary of n bytes of
// plain text, so JSON adds no escapes.
func withSummary(t *testing.T, n int) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "apps", "exec", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["summary"] = strings.Repeat("x", n)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The announcer of a manifest, and the announcer of arc serve, refuse an
// announcement above store.MaxEvent before they sign it,
// docs/delivery/SPEC.md section 4.7. An announcement of exactly that size
// is signed, and its signed JSON has the size that was checked.
func TestAnAnnouncementAboveTheLimitIsRefused(t *testing.T) {
	k := keys.Generate()
	announcers := map[string]func(data []byte) (nostr.Event, error){
		"manifest": func(data []byte) (nostr.Event, error) {
			return catalog.AnnounceManifest(context.Background(), k, data, nostr.Now())
		},
		"provider": func(data []byte) (nostr.Event, error) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			p, err := capability.LoadProvider(path)
			if err != nil {
				t.Fatal(err)
			}
			return catalog.AnnounceProvider(context.Background(), k, p, nostr.Now())
		},
	}
	for name, announce := range announcers {
		small, err := announce(withSummary(t, 1))
		if err != nil {
			t.Fatal(err)
		}
		n := store.MaxEvent - len(small.String()) + 1
		largest, err := announce(withSummary(t, n))
		if err != nil {
			t.Fatalf("%s: the largest announcement: %v", name, err)
		}
		if size := len(largest.String()); size != store.MaxEvent {
			t.Fatalf("%s: the largest announcement holds %d bytes, not %d", name, size, store.MaxEvent)
		}
		if _, err := catalog.Read(largest); err != nil {
			t.Errorf("%s: the largest announcement does not read: %v", name, err)
		}
		_, err = announce(withSummary(t, n+1))
		if !errors.Is(err, catalog.ErrTooLarge) || !strings.Contains(err.Error(), "65537 bytes") {
			t.Errorf("%s: an announcement of %d bytes: %v", name, store.MaxEvent+1, err)
		}
	}
}
