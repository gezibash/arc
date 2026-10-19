package files

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A sender that ends before the last byte leaves a good start of the file.
// The part file must stay, so that the next get takes only the rest; only
// all the bytes with a wrong SHA-256 remove it.
func TestAnEarlyEndKeepsThePartFile(t *testing.T) {
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(t.TempDir(), "got.bin")
	p, err := OpenPart(path, hex.EncodeToString(sum[:]), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(data[:100000]); err != nil {
		t.Fatal(err)
	}
	err = p.Finish()
	if err == nil || errors.Is(err, ErrWrongBytes) {
		t.Fatalf("Finish after 100000 of %d bytes gave %v, want an early end", len(data), err)
	}
	info, statErr := os.Stat(path + ".part")
	if statErr != nil || info.Size() != 100000 {
		t.Fatalf("the part file after an early end: %v, %v", info, statErr)
	}

	// The next get takes the rest, and the file is whole.
	p, err = OpenPart(path, hex.EncodeToString(sum[:]), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Have() != 100000 {
		t.Fatalf("the part file holds %d bytes, want 100000", p.Have())
	}
	if err := p.Write(data[100000:]); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(); err != nil {
		t.Fatalf("Finish of the whole file: %v", err)
	}

	// All the bytes with a wrong SHA-256 remove the part file.
	wrong := filepath.Join(t.TempDir(), "wrong.bin")
	p, err = OpenPart(wrong, hex.EncodeToString(sum[:]), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	bad := make([]byte, len(data))
	if err := p.Write(bad); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(); !errors.Is(err, ErrWrongBytes) {
		t.Fatalf("Finish of wrong bytes gave %v, want ErrWrongBytes", err)
	}
	if _, err := os.Stat(wrong + ".part"); !os.IsNotExist(err) {
		t.Fatalf("the part file of wrong bytes stayed: %v", err)
	}
}
