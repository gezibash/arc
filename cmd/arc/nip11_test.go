package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/gezibash/arc/adapters/relay/limits"
)

// Each NIP-11 document of arc relay serve lists its NIPs, though many
// requests come at once (#425: khatru appends NIPs to a copy that shares the
// slice of the relay, so two requests wrote one slot; the race detector saw
// it on CI).
func TestNIP11RequestsAtOnceDoNotRace(t *testing.T) {
	rl, _, _, closeStore, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStore)
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)

	var wg sync.WaitGroup
	docs := make(chan []any, 50)
	for range 50 {
		wg.Go(func() {
			request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
			request.Header.Set("Accept", "application/nostr+json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			var doc struct {
				SupportedNIPs []any `json:"supported_nips"`
			}
			if err := json.NewDecoder(response.Body).Decode(&doc); err != nil {
				t.Error(err)
				return
			}
			docs <- doc.SupportedNIPs
		})
	}
	wg.Wait()
	close(docs)
	for nips := range docs {
		for _, want := range []string{"9", "45", "77"} {
			found := slices.ContainsFunc(nips, func(n any) bool {
				switch v := n.(type) {
				case string:
					return v == want
				case float64:
					return want == jsonNumber(v)
				}
				return false
			})
			if !found {
				t.Fatalf("a document lists %v, without NIP %s", nips, want)
			}
		}
	}
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
