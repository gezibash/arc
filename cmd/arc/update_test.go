package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSuccessorRefusesAChannelFileWithoutASequence(t *testing.T) {
	target := filepath.Join(t.TempDir(), "stable.json")
	if err := os.WriteFile(target, []byte(`{"publisher":"p"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unsigned := map[string]any{"publisher": "p", "sequence": json.Number("2")}
	if err := checkSuccessor(target, unsigned); err == nil {
		t.Fatal("accepted a channel file without a sequence")
	}
}
