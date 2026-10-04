package bleve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/search"
)

func TestIndexPersistsUpdatesDeletesAndScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.bleve")
	idx := New(path)
	loads := 0
	docs := map[string]search.Document{
		"one":   {Title: "Routing", Text: "blue birds travel over radio"},
		"two":   {Title: "Blue", Text: "birds travel over cable"},
		"three": {Title: "French", Text: "réseau mémoire sqlite"},
	}
	req := search.Request{Namespace: "alice/journal", Query: "birds", Limit: 20,
		Sources: []search.Source{{ID: "one", Version: "v1", Selected: true}, {ID: "two", Version: "v1", Selected: true}, {ID: "three", Version: "v1", Selected: true}},
		Load: func(_ context.Context, source search.Source) (search.Document, error) {
			loads++
			return docs[source.ID], nil
		},
	}
	check := func(q string, syntax bool, want ...string) {
		t.Helper()
		req.Query, req.Syntax = q, syntax
		hits, err := idx.Search(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, hit := range hits {
			got[hit.ID] = true
			if hit.Score <= 0 {
				t.Fatalf("invalid score: %+v", hit)
			}
		}
		if len(hits) != len(want) {
			t.Fatalf("%q: got %v want %v", q, got, want)
		}
		for _, id := range want {
			if !got[id] {
				t.Fatalf("%q: missing %s in %v", q, id, got)
			}
		}
	}
	check("birds", false, "one", "two")
	if loads != 3 {
		t.Fatalf("first index loaded %d documents", loads)
	}
	idx.Close()
	idx = New(path)
	defer idx.Close()
	check("birds", false, "one", "two")
	if loads != 3 {
		t.Fatalf("reopen reloaded unchanged documents: %d", loads)
	}
	check(`"blue birds"`, true, "one")
	check("birdd~1", true, "one", "two")
	check("+birds -radio", true, "two")
	check("title:routing", true, "one")
	check("RÉSEAU", false, "three")
	check("réseau sqlite", false, "three")
	check("the", false)
	req.Sources[0].Selected = false
	req.Limit = 1
	check("birds", false, "two")
	req.Sources[0].Selected = true
	docs["one"] = search.Document{Title: "Replacement", Text: "turtles swim"}
	req.Sources[0].Version = "older-timestamp-arriving-now"
	check("birds", false, "two")
	check("turtles", false, "one")
	if loads != 4 {
		t.Fatalf("replacement loads = %d, want 4", loads)
	}
	req.Sources = req.Sources[1:]
	check("turtles", false)
	other := req
	other.Namespace = "bob/journal"
	other.Sources = []search.Source{{ID: "two", Version: "different", Selected: true}}
	other.Query = "secret"
	other.Load = func(context.Context, search.Source) (search.Document, error) {
		return search.Document{Text: "secret"}, nil
	}
	if hits, err := idx.Search(context.Background(), other); err != nil || len(hits) != 1 {
		t.Fatalf("other namespace: %v %v", hits, err)
	}
	check("secret", false)
	if err := idx.lease.Do(func(raw *diskIndex) error {
		count, err := raw.index.DocCount()
		if err != nil {
			return err
		}
		if count != 3 {
			t.Fatalf("retained %d documents, want 2 live Alice pages and 1 Bob page", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	req.Query, req.Syntax = `"unclosed`, true
	if _, err := idx.Search(context.Background(), req); err == nil {
		t.Fatal("bad syntax accepted")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private directory: %v %v", info, err)
	}
}

func TestIndexDoesNotCommitPartialRefreshOrLoadUnselectedBodies(t *testing.T) {
	idx := New("")
	defer idx.Close()
	missing := errors.New("missing source part")
	loads := 0
	req := search.Request{Namespace: "test", Query: "good",
		Sources: []search.Source{{ID: "good", Version: "1", Selected: true}, {ID: "bad", Version: "1", Selected: false}},
		Load: func(_ context.Context, s search.Source) (search.Document, error) {
			loads++
			if s.ID == "bad" {
				return search.Document{}, missing
			}
			return search.Document{Text: "good"}, nil
		},
	}
	if hits, err := idx.Search(context.Background(), req); err != nil || len(hits) != 1 {
		t.Fatalf("unselected source blocked search: %v %v", hits, err)
	}
	req.Sources[0].Version = "2"
	req.Sources[1].Selected = true
	if hits, err := idx.Search(context.Background(), req); !errors.Is(err, missing) || len(hits) != 0 {
		t.Fatalf("partial refresh returned results: %v %v", hits, err)
	}
	req.Sources[1].Selected = false
	if _, err := idx.Search(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if loads != 4 {
		t.Fatalf("failed batch retained a source version: loaded %d, want 4", loads)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := idx.Search(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestTwoIndexInstancesReleaseTheFileBetweenCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.bleve")
	a, b := New(path), New(path)
	defer a.Close()
	defer b.Close()
	req := search.Request{Namespace: "shared", Query: "same", Sources: []search.Source{{ID: "1", Version: "1", Selected: true}},
		Load: func(context.Context, search.Source) (search.Document, error) {
			return search.Document{Text: "same"}, nil
		},
	}
	if _, err := a.Search(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Load = func(context.Context, search.Source) (search.Document, error) {
		return search.Document{}, errors.New("unchanged source loaded")
	}
	done := make(chan error, 1)
	go func() {
		hits, err := b.Search(context.Background(), req)
		if err == nil && len(hits) != 1 {
			err = errors.New("shared result missing")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another command cannot open the search index")
	}
}

func TestConcurrentRefreshesKeepAllNamespaces(t *testing.T) {
	idx := New("")
	defer idx.Close()
	var wg sync.WaitGroup
	for _, namespace := range []string{"a", "b", "c"} {
		wg.Go(func() {
			req := search.Request{Namespace: namespace, Query: namespace, Sources: []search.Source{{ID: "same", Version: "1", Selected: true}},
				Load: func(context.Context, search.Source) (search.Document, error) {
					return search.Document{Text: namespace}, nil
				},
			}
			for range 5 {
				if hits, err := idx.Search(context.Background(), req); err != nil || len(hits) != 1 {
					t.Errorf("%s: %v %v", namespace, hits, err)
				}
			}
		})
	}
	wg.Wait()
}

func TestConcurrentFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.bleve")
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		idx := New(path)
		t.Cleanup(idx.Close)
		wg.Go(func() {
			<-ready
			req := search.Request{Namespace: "shared", Query: "sqlite", Sources: []search.Source{{ID: "1", Version: "1", Selected: true}},
				Load: func(context.Context, search.Source) (search.Document, error) {
					return search.Document{Text: "sqlite"}, nil
				},
			}
			if hits, err := idx.Search(context.Background(), req); err != nil || len(hits) != 1 {
				t.Errorf("first open: %v %v", hits, err)
			}
		})
	}
	close(ready)
	wg.Wait()
}
