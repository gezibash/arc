package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// syncs records each sync, and checks a path at that time: at a sync of a
// file, final must not be there yet; at a sync of a directory, it must.
type syncs struct {
	final string
	calls []string
}

func watchSyncs(t *testing.T, final string) *syncs {
	t.Helper()
	s := &syncs{final: final}
	fs, ds := fileSync, dirSync
	t.Cleanup(func() { fileSync, dirSync = fs, ds })
	fileSync = func(f *os.File) error {
		s.calls = append(s.calls, "file")
		if s.final != "" {
			if _, err := os.Stat(s.final); err == nil {
				t.Errorf("%s is in place before its bytes are synced", filepath.Base(s.final))
			}
		}
		return f.Sync()
	}
	dirSync = func(dir string) error {
		s.calls = append(s.calls, "dir "+dir)
		return syncDir(dir)
	}
	return s
}

func wholePart(t *testing.T, path string) *Part {
	t.Helper()
	data := []byte("the bytes of a file")
	sum := sha256.Sum256(data)
	p, err := OpenPart(path, hex.EncodeToString(sum[:]), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(data); err != nil {
		t.Fatal(err)
	}
	return p
}

// Finish syncs the bytes before the rename, and the directory after it.
func TestFinishSyncsTheFileThenTheName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "got.bin")
	p := wholePart(t, path)
	s := watchSyncs(t, path)
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"file", "dir " + dir}; !slices.Equal(s.calls, want) {
		t.Fatalf("Finish synced %q, want %q", s.calls, want)
	}
}

// A directory that cannot be synced after a good rename does not undo the
// file: Finish ends well, and the file is in place.
func TestAFailedDirSyncAfterTheRenameIsNoRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "got.bin")
	p := wholePart(t, path)
	ds := dirSync
	t.Cleanup(func() { dirSync = ds })
	dirSync = func(string) error { return errors.New("the directory cannot be synced") }
	if err := p.Finish(); err != nil {
		t.Fatalf("Finish gave %v with the file in place", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// WriteFile syncs the bytes before the rename, and the directory after it.
func TestWriteFileSyncsTheFileThenTheName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "offer.json")
	s := watchSyncs(t, path)
	if err := WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if want := []string{"file", "dir " + dir}; !slices.Equal(s.calls, want) {
		t.Fatalf("WriteFile synced %q, want %q", s.calls, want)
	}
}

// MkdirSynced puts each new directory in its parent on the disk, from the
// top down, and syncs nothing that was there.
func TestMkdirSyncedSyncsEachNewName(t *testing.T) {
	root := t.TempDir()
	s := watchSyncs(t, "")
	dir := filepath.Join(root, "state", "delivered")
	if err := MkdirSynced(dir); err != nil {
		t.Fatal(err)
	}
	if want := []string{"dir " + root, "dir " + filepath.Join(root, "state")}; !slices.Equal(s.calls, want) {
		t.Fatalf("MkdirSynced synced %q, want %q", s.calls, want)
	}
	s.calls = nil
	if err := MkdirSynced(dir); err != nil || len(s.calls) != 0 {
		t.Fatalf("MkdirSynced of a directory that is there: %v, synced %q", err, s.calls)
	}
}

// A move to another file system copies through a part file whose bytes are
// on the disk before its rename, and syncs the directory after it.
func TestACopiedMoveSyncsTheFileThenTheName(t *testing.T) {
	src := filepath.Join(t.TempDir(), "received")
	if err := os.WriteFile(src, []byte("the bytes of a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "got.bin")
	s := watchSyncs(t, dst)
	if err := copyThroughPart(src, dst); err != nil {
		t.Fatal(err)
	}
	if want := []string{"file", "dir " + dir}; !slices.Equal(s.calls, want) {
		t.Fatalf("the copy synced %q, want %q", s.calls, want)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "the bytes of a file" {
		t.Fatalf("the copy holds %q (%v)", got, err)
	}
}

// A move on one file system renames, and syncs the directory of dst.
func TestAMoveSyncsTheName(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "received"), filepath.Join(dir, "got.bin")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := watchSyncs(t, "")
	if err := MoveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if want := []string{"dir " + dir}; !slices.Equal(s.calls, want) {
		t.Fatalf("the move synced %q, want %q", s.calls, want)
	}
}
