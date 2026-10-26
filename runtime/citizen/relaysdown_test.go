package citizen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A file of down relays that is missing, does not parse, or lies in a
// folder that arc cannot write, is no record: nothing fails (#439).
func TestABrokenFileOfDownRelaysIsNoRecord(t *testing.T) {
	dir := t.TempDir()
	d := relaysDown{path: filepath.Join(dir, "relays-down.json")}
	if _, _, ok := d.Down("ws://a"); ok {
		t.Fatal("a missing file holds a record")
	}
	if err := os.WriteFile(d.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.Down("ws://a"); ok {
		t.Fatal("a file that does not parse holds a record")
	}
	d.MarkDown("ws://a", time.Now().Add(time.Minute), "down")
	if _, why, ok := d.Down("ws://a"); !ok || why != "down" {
		t.Fatalf("a record over a broken file did not keep: %q, %v", why, ok)
	}
	if os.Geteuid() != 0 {
		locked := relaysDown{path: filepath.Join(dir, "locked", "relays-down.json")}
		if err := os.MkdirAll(filepath.Dir(locked.path), 0o500); err != nil {
			t.Fatal(err)
		}
		locked.MarkDown("ws://a", time.Now().Add(time.Minute), "down")
		if _, _, ok := locked.Down("ws://a"); ok {
			t.Error("a folder that arc cannot write holds a record")
		}
	}
}

// Many processes that mark relays down at once never leave a file that
// does not parse. A record ends by the wall clock.
func TestDownRelaysOfManyProcessesKeepAWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relays-down.json")
	var wg sync.WaitGroup
	var torn sync.Map
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if body, err := os.ReadFile(path); err == nil && !json.Valid(body) {
					torn.Store(string(body), true)
				}
			}
		})
	}
	var writers sync.WaitGroup
	for i := range 20 {
		writers.Go(func() {
			d := relaysDown{path: path}
			for j := range 20 {
				d.MarkDown(fmt.Sprintf("ws://%d-%d", i, j), time.Now().Add(time.Minute), "down")
				d.Up(fmt.Sprintf("ws://%d-%d", i, j-1))
			}
		})
	}
	writers.Wait()
	close(stop)
	wg.Wait()
	torn.Range(func(body, _ any) bool {
		t.Errorf("a reader saw a file that does not parse: %q", body)
		return false
	})
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]downRecord
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatalf("the file does not parse after many writers: %v\n%s", err, body)
	}
	d := relaysDown{path: path}
	d.MarkDown("ws://old", time.Now().Add(-time.Second), "down")
	if _, ok := d.read()["ws://old"]; ok {
		t.Error("a record whose time passed stays in the file")
	}
}
