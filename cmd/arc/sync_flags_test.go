package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arc sync --repair --dir is refused: a directory holds no pieces to
// repair, and the pair silently did a plain sync before (#404).
func TestSyncRefusesRepairWithADirectory(t *testing.T) {
	home := user(t)
	identity(t, filepath.Join(home, ".arc"))
	dir := t.TempDir()
	_, _, err := runErr(t, "sync", "--repair", "--dir", dir)
	if err == nil || !strings.Contains(err.Error(), "run arc sync --repair and arc sync --dir apart") {
		t.Errorf("sync --repair --dir gave %v, want a refusal", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused sync wrote %v to the directory", entries)
	}
}
