package proof

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bodyCommit is v0.30.0, the last arc that keeps a file in parts.
const bodyCommit = "9ee6088ae26a7ee6e05e422a9e822c2f854e708a"

// TestStorageLargeFile proves #247 through the normal CLI, a real relay
// and three storage citizens. A file of 60 MiB goes to the storage
// citizens as one blob of raw bytes, coded into pieces. The pieces hold
// at most twice the file. A second machine reads the file back with the
// same SHA-256. A file that v0.30.0 put in parts still reads. A machine
// that runs v0.30.0 refuses the new blob, and gets it after its update.
func TestStorageLargeFile(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	laptop := arc(t, filepath.Join(work, "laptop")).with(loopback...)
	laptop.run("keys", "gen")
	laptop.run("relay", "add", url)
	citizen := laptop.key()
	stores := map[string]string{}
	for _, name := range []string{"charlie", "dana", "erin"} {
		key, _ := servedStorageCitizen(t, work, url, laptop, name, 0)
		has(t, laptop.run("keys", "storage", "add", key, "--as", name), "storage list: "+name+" is active", "the add of "+name)
		stores[name] = filepath.Join(work, name+"-blobs")
	}
	held := func() int64 {
		var n int64
		for _, dir := range stores {
			for _, path := range pieceFiles(t, dir) {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				n += info.Size()
			}
		}
		return n
	}

	// v0.30.0 puts a file in parts, with the manifest of that version.
	olderArc := olderProgramAt(t, bodyCommit, "./cmd/arc")
	olderManifest := filepath.Join(work, "files-0.30.0.json")
	shown, err := exec.Command("git", "-C", repo, "show", bodyCommit+":apps/files/manifest.json").Output()
	if err != nil {
		t.Fatal(err)
	}
	write(t, olderManifest, string(shown), 0o600)
	older := laptop.using(olderArc)
	older.run("announce", olderManifest)
	older.run("install", citizen, "files", "--yes")
	small := randomFile(t, filepath.Join(work, "notes.bin"), 100_000)
	smallID := ifaceTrim(older.run("files", "put", small.path))
	older.run("sync")
	t.Log("ok: v0.30.0 puts a file of 100,000 bytes in parts, and syncs it")

	// This version puts a file of 60 MiB as one body.
	laptop.run("announce", filepath.Join(repo, "apps/files/manifest.json"))
	laptop.run("install", citizen, "files", "--yes")
	large := randomFile(t, filepath.Join(work, "model.bin"), 60<<20)
	before := held()
	largeID := ifaceTrim(laptop.run("files", "put", large.path))
	if largeID != large.sum {
		t.Fatalf("put printed %q, want the SHA-256 %s", largeID, large.sum)
	}
	up := laptop.run("sync")
	has(t, up, "2 of 3 rebuild each", "the sync of the laptop")
	added := held() - before
	// A blob of 64 MiB in three pieces of 32 MiB + 45 bytes, and a few
	// small blobs: the checkpoint and the deletion of the parts.
	t.Logf("measured: a file of %d bytes added %d bytes on the storage citizens, %.2f times the file", large.size, added, float64(added)/float64(large.size))
	if added > 2*large.size {
		t.Fatalf("the storage citizens hold %d more bytes for a file of %d, more than twice", added, large.size)
	}
	for name, dir := range stores {
		for _, path := range pieceFiles(t, dir) {
			if data, err := os.ReadFile(path); err == nil && containsChunk(data, large.data) {
				t.Fatalf("a piece on %s holds raw bytes of the file", name)
			}
		}
	}
	t.Log("ok: the file goes to the storage citizens as one blob in pieces, at most twice its size")

	// A second machine of the citizen reads both files back.
	desktop := arc(t, filepath.Join(work, "desktop")).with(loopback...)
	desktop.input(read(t, keyfile(t, laptop.home)), "keys", "add")
	desktop.run("relay", "add", url)
	desktop.run("install", citizen, "files", "--yes")
	desktop.run("sync")
	for _, f := range []struct {
		id   string
		file randomBytes
	}{{largeID, large}, {smallID, small}} {
		out := filepath.Join(work, "desktop-"+filepath.Base(f.file.path))
		desktop.run("files", "get", f.id, "--output", out)
		if got := sha256File(t, out); got != f.file.sum {
			t.Fatalf("the desktop wrote %s with the SHA-256 %s, want %s", out, got, f.file.sum)
		}
	}
	t.Log("ok: a second machine reads the file of 60 MiB and the file in parts with the same SHA-256")

	// A device signs with its device key through the bunker of the laptop,
	// and opens the body with the data roots that the signer gives it.
	const limit = 10 * time.Second
	phone := arc(t, filepath.Join(work, "phone")).with(loopback...)
	phoneDevice := ifaceTrim(phone.run("keys", "device"))
	bunker := laptop.start("keys", "bunker", "--relay", url)
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	has(t, laptop.run("keys", "devices", "add", phoneDevice, "--name", "phone"), "grants: sent to the storage citizen", "the add of the phone")
	if !loads(bunker, 1, limit) {
		t.Fatalf("the bunker did not load the device list:\n%s", bunker.output())
	}
	phone.run("keys", "add", "bunker://"+uri)
	phone.run("relay", "add", url)
	phone.run("install", citizen, "files", "--yes")
	phone.run("sync")
	out := filepath.Join(work, "phone-model.bin")
	phone.run("files", "get", largeID, "--output", out)
	if got := sha256File(t, out); got != large.sum {
		t.Fatalf("the phone wrote the SHA-256 %s, want %s", got, large.sum)
	}
	t.Log("ok: a device behind the bunker reads the file of 60 MiB with the same SHA-256")

	// A machine that runs v0.30.0 refuses the blob of version 2, and
	// keeps no wrong file. After its update it gets the blob.
	old := arc(t, filepath.Join(work, "old")).with(loopback...).using(olderArc)
	old.input(read(t, keyfile(t, laptop.home)), "keys", "add")
	old.run("relay", "add", url)
	refused := old.try("", "sync")
	has(t, refused.Stderr, "no envelope of version 1", "the sync of v0.30.0")
	// It cannot parse the new manifest, so it skips the announcement.
	old.run("discover")
	has(t, old.refuse("install", citizen, "files", "--yes"), "no announcement from that provider", "the install of the new manifest by v0.30.0")
	updated := old.using(program("arc"))
	updated.run("sync")
	updated.run("install", citizen, "files", "--yes")
	out = filepath.Join(work, "updated-model.bin")
	updated.run("files", "get", largeID, "--output", out)
	if got := sha256File(t, out); got != large.sum {
		t.Fatalf("the updated machine wrote the SHA-256 %s, want %s", got, large.sum)
	}
	t.Log("ok: v0.30.0 refuses the new blob; after its update the machine gets the file")

	// A directory that the citizen carries takes the body beside its wrap.
	stick := filepath.Join(work, "stick")
	has(t, laptop.run("sync", "--dir", stick), "bodies received 0, sent 1", "the sync of the laptop with the stick")
	carried := arc(t, filepath.Join(work, "carried")).with(loopback...)
	carried.input(read(t, keyfile(t, laptop.home)), "keys", "add")
	carried.run("relay", "add", url)
	carried.run("discover")
	carried.run("install", citizen, "files", "--yes")
	has(t, carried.run("sync", "--dir", stick), "bodies received 1, sent 0", "the sync of a machine with the stick")
	out = filepath.Join(work, "carried-model.bin")
	carried.run("files", "get", largeID, "--output", out)
	if got := sha256File(t, out); got != large.sum {
		t.Fatalf("the machine with the stick wrote the SHA-256 %s, want %s", got, large.sum)
	}
	t.Log("ok: arc sync --dir carries the body, and a machine reads the file from the stick alone")

	// A deletion removes the pieces of the blob from the storage citizens.
	// The first repair puts the deletion; the next one deletes the pieces,
	// once the storage citizens hold the deletion (storage spec, 4.7).
	before = held()
	laptop.run("files", "delete", largeID)
	laptop.run("sync", "--repair")
	has(t, laptop.run("sync", "--repair"), "removed 2 blobs of replaced records", "the second repair after the deletion")
	freed := before - held()
	t.Logf("measured: the deletion freed %d bytes on the storage citizens", freed)
	// The blank draft and the deletion add a few small pieces.
	if freed < added-(1<<20) {
		t.Fatalf("the deletion freed %d bytes; the file added %d", freed, added)
	}
	t.Log("ok: a deletion of the file removes its pieces")
}

type randomBytes struct {
	path string
	data []byte
	size int64
	sum  string
}

func randomFile(t *testing.T, path string, n int) randomBytes {
	t.Helper()
	data := make([]byte, n)
	rand.Read(data)
	write(t, path, string(data), 0o600)
	sum := sha256.Sum256(data)
	return randomBytes{path: path, data: data, size: int64(n), sum: hex.EncodeToString(sum[:])}
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// containsChunk says whether data holds the first 64 bytes of raw.
func containsChunk(data, raw []byte) bool {
	return strings.Contains(string(data), string(raw[:64]))
}
