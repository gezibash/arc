package bodies

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sumOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestPruneKeepsNamedAndYoungBodies checks that prune removes only a body
// that no wrap names and that is older than the grace, and that a body
// that changed on the disk does not read.
func TestPruneKeepsNamedAndYoungBodies(t *testing.T) {
	d := Dir{Path: t.TempDir()}
	named, orphan, young := []byte("named"), []byte("orphan"), []byte("young")
	for _, b := range [][]byte{named, orphan, young} {
		if err := d.Put(sumOf(b), b); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Put(sumOf(named), []byte("other bytes")); err == nil {
		t.Error("Put kept bytes under another sum")
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, b := range [][]byte{named, orphan} {
		if err := os.Chtimes(filepath.Join(d.Path, sumOf(b)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.Prune(map[string]bool{sumOf(named): true}, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune removed %d bodies: %v; want 1", n, err)
	}
	if d.Has(sumOf(orphan)) || !d.Has(sumOf(named)) || !d.Has(sumOf(young)) {
		t.Fatal("prune removed another body than the old one that no wrap names")
	}

	if err := os.WriteFile(filepath.Join(d.Path, sumOf(named)), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(sumOf(named)); err == nil {
		t.Error("a damaged body read")
	}
}
