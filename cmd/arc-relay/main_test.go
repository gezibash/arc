package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnOlderStoreIsNotLeftBehind checks that a relay with no --db does not
// start on a new, empty store when an older arc kept its store in
// ~/.config/arc: the events there would seem lost.
func TestAnOlderStoreIsNotLeftBehind(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("ARC_HOME", "")
	if path, err := defaultDB(); err != nil || path != filepath.Join(user, ".arc", "relay.db") {
		t.Fatalf("a new machine: %q, %v", path, err)
	}
	older := filepath.Join(user, ".config", "arc", "relay.db")
	if err := os.MkdirAll(filepath.Dir(older), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(older, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultDB(); err == nil || !strings.Contains(err.Error(), "--db "+older) {
		t.Fatalf("with only an older store: %v, want it named", err)
	}
	t.Setenv("ARC_HOME", filepath.Join(user, "home"))
	if path, err := defaultDB(); err != nil || path != filepath.Join(user, "home", "relay.db") {
		t.Fatalf("ARC_HOME: %q, %v", path, err)
	}
}
