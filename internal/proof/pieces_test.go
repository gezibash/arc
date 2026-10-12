package proof

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/storage"
)

// olderCommit is the last commit of main before the pieces. Its storage
// app keeps no tags, and holds whole blobs of format 1.
const olderCommit = "d53e91bddc9d2b90686c87ee0f523f32f48668e2"

// TestStoragePieces proves storage network step 6 through the normal CLI,
// a real relay, three storage citizens of this version and one of the
// older version. A signer codes each record into 3 pieces, 2 of which
// rebuild it, one on each storage citizen of this version. The older one
// gets whole blobs. A machine from the seed rebuilds each page from 2
// pieces, and refuses a piece with a changed byte. With 2 of 3 storage
// citizens stopped, it fails, and says why. It still reads the whole blobs
// of the older storage citizen.
func TestStoragePieces(t *testing.T) {
	begin := time.Now()
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	older := olderStorageApp(t)

	signer := arc(t, filepath.Join(work, "signer")).with(loopback...)
	made := signer.try("", "keys", "gen")
	if made.Err != nil {
		t.Fatalf("arc keys gen: %v\n%s", made.Err, made.Stderr)
	}
	words := seedWords(made.Stderr)
	signer.run("relay", "add", url)
	citizen := signer.key()
	names := []string{"charlie", "dana", "erin"}
	serve := map[string]func() *process{}
	running := map[string]*process{}
	for _, name := range append(names, "frank") {
		app := program("arc-storage")
		if name == "frank" {
			app = older
		}
		var key string
		key, serve[name] = storageServer(t, work, url, signer, name, app, 0)
		running[name] = serve[name]()
		has(t, signer.run("keys", "storage", "add", key, "--as", name), "storage list: "+name+" is active", "the add of "+name)
	}
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")

	pages := map[string]string{}
	for n := 1; n <= 4; n++ {
		page := fmt.Sprintf("hrs/rs/%d", n)
		pages[page] = fmt.Sprintf("the text of page %d in pieces", n)
		signer.input(fmt.Sprintf("---\ntitle: Page %d\npage: %d\nnotebook: hrs/rs\n---\n%s\n", n, n, pages[page]), "journal", "write", page)
	}
	start := time.Now()
	first := signer.try("", "sync")
	t.Logf("measured: the first arc sync with three storage citizens and an older one took %s", time.Since(start).Round(time.Millisecond))
	if first.Err != nil {
		t.Fatalf("arc sync: %v\n%s\n%s", first.Err, first.Stdout, first.Stderr)
	}
	has(t, first.Stdout, "2 of 3 rebuild each", "the code of the first sync")
	has(t, first.Stderr, "frank runs an older storage app: it holds full copies; update it for erasure coding", "the sync with the older storage app")
	t.Logf("first sync:\n%s", strings.TrimSpace(first.Stdout))

	// Each storage citizen of this version holds one piece of each blob.
	// A piece is half its blob, plus 45 bytes, and holds no text.
	files := map[string][]string{}
	for _, name := range names {
		files[name] = pieceFiles(t, filepath.Join(work, name+"-blobs"))
		for _, path := range files[name] {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// The pages make blobs of 4 KiB. A shard of 2 KiB holds half of
			// one, and no shard can hold a whole blob.
			shard := int64(len(data)) - storage.PieceOverhead
			if shard >= storage.MinBlob || !storage.GoodSize(2*shard) {
				t.Fatalf("%s: a piece of %d bytes is not half of a blob plus %d bytes", name, len(data), storage.PieceOverhead)
			}
			for _, text := range pages {
				if bytes.Contains(data, []byte(text)) {
					t.Fatalf("%s: the piece %s holds the text of a page", name, path)
				}
			}
		}
	}
	n := len(files["charlie"])
	if n == 0 || len(files["dana"]) != n || len(files["erin"]) != n {
		t.Fatalf("the storage citizens hold %d, %d and %d pieces; want one piece of each blob on each", n, len(files["dana"]), len(files["erin"]))
	}
	for _, path := range pieceFiles(t, filepath.Join(work, "frank-blobs")) {
		if info, err := os.Stat(path); err != nil || !storage.GoodSize(info.Size()) {
			t.Fatalf("frank holds %s, which is not a whole blob: %v", path, err)
		}
	}
	t.Logf("ok: each of three storage citizens holds one piece of each of %d blobs, half its blob plus 45 bytes, with no text; the older one holds whole blobs", n)

	// Decision 1: each storage citizen has its own piece ID key, so two
	// storage citizens show no common piece ID. Each piece has its own
	// tag.
	ids, tags := map[string]string{}, map[string]string{}
	for _, name := range names {
		for _, path := range files[name] {
			id := filepath.Base(path)
			if other, ok := ids[id]; ok {
				t.Fatalf("%s and %s hold the piece ID %s", name, other, id)
			}
			ids[id] = name
		}
		entries := listAll(t, signer, name)
		if len(entries) != n {
			t.Fatalf("%s lists %d entries, and holds %d piece files", name, len(entries), n)
		}
		for _, e := range entries {
			if len(e.tag) != storage.TagSize {
				t.Fatalf("%s lists %s with a tag of %d bytes, want %d", name, e.id, len(e.tag), storage.TagSize)
			}
			if other, ok := tags[string(e.tag)]; ok {
				t.Fatalf("%s and %s list the same tag", name, other)
			}
			tags[string(e.tag)] = name
		}
	}
	t.Log("ok: two storage citizens show no common piece ID, and each piece has its own tag of 87 bytes")

	// The older storage citizen stops. Each new machine must now rebuild
	// each page from pieces.
	running["frank"].stop()

	// One byte changes in each piece of dana. With charlie stopped, each
	// blob needs the piece of dana, so no page opens. The device says so.
	for _, path := range files["dana"] {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)/2] ^= 1
		write(t, path, string(data), 0o600)
	}
	running["charlie"].stop()
	reader := fromSeed(t, work, "reader", url, citizen, words)
	broken := reader.try("", "sync")
	if broken.Err == nil {
		t.Fatalf("arc sync succeeded with charlie stopped and each piece of dana changed:\n%s", broken.Stdout)
	}
	has(t, broken.Stderr, "the piece does not open; the device asks another storage citizen", "the sync with changed pieces")
	has(t, broken.Stderr, "cannot be read now", "the sync with changed pieces")
	for page, want := range pages {
		if got := reader.try("", "journal", "read", page); strings.Contains(got.Stdout, want) {
			t.Fatalf("the reader reads %s from one good piece:\n%s", page, got.Stdout)
		}
	}
	t.Log("ok: a piece with one changed byte does not open, and one good piece rebuilds nothing")

	// With charlie back, the device rebuilds each blob from the pieces of
	// charlie and erin.
	running["charlie"] = serve["charlie"]()
	mended := reader.try("", "sync")
	for page, want := range pages {
		has(t, journalDoc(t, begin, reader.run("journal", "read", page)), want, "the reading of "+page+" with the pieces of dana changed")
	}
	// The device asks the 2 storage citizens that rank highest for each
	// blob. Dana ranks in the first 2 for a blob with a chance of 2 in 3,
	// so with 8 blobs it is asked first, and refused, almost always.
	has(t, mended.Stderr, "the piece does not open; the device asks another storage citizen", "the sync with charlie back")
	if strings.Contains(mended.Stderr, "cannot be read now") {
		t.Fatalf("the sync with charlie back still names blobs that cannot be read:\n%s", mended.Stderr)
	}
	t.Log("ok: the device reads each page from the pieces of charlie and erin")

	// The directory of dana goes. A new machine reads every page from 2 of
	// 3 storage citizens.
	running["dana"].stop()
	if err := os.RemoveAll(filepath.Join(work, "dana-blobs")); err != nil {
		t.Fatal(err)
	}
	second := fromSeed(t, work, "second", url, citizen, words)
	second.try("", "sync")
	for page, want := range pages {
		has(t, journalDoc(t, begin, second.run("journal", "read", page)), want, "the reading of "+page+" without dana")
	}
	t.Log("ok: with the directory of dana deleted, a new machine reads every page from charlie and erin")

	// With erin stopped too, one piece of each blob answers. The sync
	// fails, and says that the blobs need 2 pieces.
	running["erin"].stop()
	third := fromSeed(t, work, "third", url, citizen, words)
	short := third.try("", "sync")
	if short.Err == nil {
		t.Fatalf("arc sync succeeded with 2 of 3 storage citizens stopped:\n%s", short.Stdout)
	}
	has(t, short.Stderr, "blobs cannot be read now: fewer of their pieces answered than rebuild them", "the sync with 2 of 3 stopped")
	has(t, short.Stderr, "1 of the 2 pieces that rebuild it answered", "the sync with 2 of 3 stopped")
	for page, want := range pages {
		if got := third.try("", "journal", "read", page); strings.Contains(got.Stdout, want) {
			t.Fatalf("the third machine reads %s from one piece:\n%s", page, got.Stdout)
		}
	}
	t.Logf("ok: with 2 of 3 stopped, the sync fails clearly:\n%s", strings.TrimSpace(short.Stderr))

	// The older storage citizen holds whole blobs of format 1. A device of
	// this version still reads them.
	running["frank"] = serve["frank"]()
	whole := third.try("", "sync")
	if got, _ := syncTook(t, "frank", whole.Stdout); got.received == 0 {
		t.Fatalf("the sync read nothing from frank:\n%s\n%s", whole.Stdout, whole.Stderr)
	}
	for page, want := range pages {
		has(t, journalDoc(t, begin, third.run("journal", "read", page)), want, "the reading of "+page+" from frank")
	}
	t.Log("ok: a device reads each page from the whole blobs of format 1 of the older storage app")
}

// fromSeed makes a new machine of the citizen from the seed, with the
// relay and the journal.
func fromSeed(t *testing.T, work, name, url, citizen string, words []string) machine {
	t.Helper()
	m := arc(t, filepath.Join(work, name)).with(loopback...)
	if got := ifaceTrim(m.input(strings.Join(words, " ")+"\n", "keys", "recover")); !strings.HasSuffix(got, citizen) {
		t.Fatalf("the seed made %q, not the citizen %s", got, citizen)
	}
	m.run("relay", "add", url)
	m.run("install", citizen, "journal", "--yes")
	return m
}

// pieceFiles returns the files of each account under a storage directory.
func pieceFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "accounts", "*", "blobs", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// olderStorageApp builds the storage app of olderCommit from the history
// of the repository, outside the checkout.
func olderStorageApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tarball := filepath.Join(dir, "src.tar")
	src := filepath.Join(dir, "src")
	bin := filepath.Join(dir, "arc-storage")
	steps := []*exec.Cmd{
		exec.Command("git", "-C", repo, "archive", "--format=tar", "-o", tarball, olderCommit),
		exec.Command("mkdir", src),
		exec.Command("tar", "-xf", tarball, "-C", src),
		exec.Command("go", "build", "-o", bin, "./apps/storage/cmd/arc-storage"),
	}
	steps[3].Dir = src
	for _, step := range steps {
		if out, err := step.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(step.Args, " "), err, out)
		}
	}
	return bin
}

// pipe opens a pipe to the installed storage app as of machine m, through
// the normal CLI: arc <as> open <session> makes the ARC call. The test then
// speaks the requests of the pipe, as an older device or a reader of the
// listings does.
func pipe(t *testing.T, m machine, as string) *storage.Conn {
	t.Helper()
	client := storage.Client{
		Call: func(_ context.Context, session string) ([]byte, error) {
			r := m.try("", as, "open", session)
			if r.Err != nil {
				return nil, fmt.Errorf("arc %s open: %v: %s", as, r.Err, r.Stderr)
			}
			return []byte(strings.TrimSpace(r.Stdout)), nil
		},
		Options: direct.Options{Loopback: true},
		NoRelay: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := client.Open(ctx, 0)
	if err != nil {
		t.Fatalf("no pipe to %s opens: %v", as, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// listed is one entry of a listing, with the bytes of its piece or blob.
type listed struct {
	id   string
	tag  []byte
	data []byte
}

// listAll lists each present entry of the account of m on the storage app
// as, and gets its bytes.
func listAll(t *testing.T, m machine, as string) []listed {
	t.Helper()
	conn := pipe(t, m, as)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entries, _, err := conn.List(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []listed
	var ids []string
	for _, e := range entries {
		if e.Deleted {
			continue
		}
		var tag []byte
		if e.Tag != "" {
			if tag, err = base64.StdEncoding.DecodeString(e.Tag); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, listed{id: e.ID, tag: tag})
		ids = append(ids, e.ID)
	}
	n := 0
	err = conn.GetMany(ctx, ids, func(_ string, data []byte, err error) error {
		if err != nil {
			return err
		}
		out[n].data = data
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
