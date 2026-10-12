package proof

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/storage"
)

// TestStorageFormat1 proves storage network step 8 through the normal CLI
// and a real relay. The arc of olderCommit puts whole blobs of format 1 on
// three storage citizens of this version. The home then updates to this
// arc, and one storage citizen drains. A plain sync moves nothing, and
// counts the blobs. With dana stopped, a repair codes each record into a
// piece on charlie, and deletes the whole copies of charlie and erin: the
// move stops halfway, and a new machine still reads every page. With dana
// back, the next repair ends the move. Then each storage citizen holds
// pieces only, erin holds nothing, and a new machine reads every page from
// charlie alone and from dana alone.
func TestStorageFormat1(t *testing.T) {
	begin := time.Now()
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	olderArc := olderProgram(t, "./cmd/arc")

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
	for _, name := range names {
		var key string
		key, serve[name] = storageServer(t, work, url, signer, name, program("arc-storage"), 0)
		running[name] = serve[name]()
		has(t, signer.run("keys", "storage", "add", key, "--as", name), "storage list: "+name+" is active", "the add of "+name)
	}
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")
	pages := map[string]string{}
	for n := 1; n <= 4; n++ {
		page := fmt.Sprintf("hrs/old/%d", n)
		pages[page] = fmt.Sprintf("the text of page %d in a whole blob", n)
		signer.input(fmt.Sprintf("---\ntitle: Page %d\npage: %d\nnotebook: hrs/old\n---\n%s\n", n, n, pages[page]), "journal", "write", page)
	}

	// The older arc puts whole blobs of format 1, with no tag.
	older := signer.using(olderArc)
	if got := older.try("", "sync"); got.Err != nil {
		t.Fatalf("the older arc sync: %v\n%s\n%s", got.Err, got.Stdout, got.Stderr)
	}
	n := 0
	for _, name := range names {
		entries := listAll(t, signer, name)
		for _, e := range entries {
			if e.tag != nil || !storage.GoodSize(int64(len(e.data))) {
				t.Fatalf("%s holds %s of %d bytes with a tag of %d bytes; want a whole blob with no tag", name, e.id, len(e.data), len(e.tag))
			}
		}
		if n == 0 {
			n = len(entries)
		}
		if n == 0 || len(entries) != n {
			t.Fatalf("%s holds %d whole blobs; want %d, more than 0", name, len(entries), n)
		}
	}
	t.Logf("ok: the older arc put %d whole blobs of format 1 on each of three storage citizens", n)

	// The home updates. Erin drains. A sync with no --repair moves
	// nothing, and says how many blobs remain.
	has(t, signer.run("keys", "storage", "rm", "erin"), "draining", "the rm of erin")
	plain := signer.try("", "sync")
	if plain.Err != nil {
		t.Fatalf("arc sync: %v\n%s\n%s", plain.Err, plain.Stdout, plain.Stderr)
	}
	has(t, plain.Stdout, fmt.Sprintf("self data: %d blobs of format 1 remain; repair codes them into pieces", n), "a sync with no --repair")
	has(t, plain.Stderr, "blobs need repair", "a sync with no --repair")
	for _, name := range names {
		if got := len(listAll(t, signer, name)); got != n {
			t.Fatalf("a sync with no --repair changed %s: it lists %d entries, not %d", name, got, n)
		}
	}
	t.Log("ok: a sync with no --repair moves nothing, and counts the blobs of format 1")

	readsAll := func(name, what string) {
		t.Helper()
		m := fromSeed(t, work, name, url, citizen, words)
		got := m.try("", "sync")
		t.Logf("a new machine %s:\n%s\n%s", what, strings.TrimSpace(got.Stdout), strings.TrimSpace(got.Stderr))
		for page, want := range pages {
			has(t, journalDoc(t, begin, m.run("journal", "read", page)), want, "the reading of "+page+" "+what+"\n"+got.Stdout+got.Stderr)
		}
	}

	// Dana stops. The repair codes each record into a piece on charlie,
	// and deletes the whole copies on charlie and erin. Dana keeps its
	// whole copies, so the move is not done.
	running["dana"].stop()
	half := signer.try("", "sync", "--repair")
	t.Logf("arc sync --repair with dana stopped:\n%s\n%s", strings.TrimSpace(half.Stdout), strings.TrimSpace(half.Stderr))
	has(t, half.Stdout, fmt.Sprintf("coded %d blobs of format 1 into pieces", n), "the repair with dana stopped")
	has(t, half.Stdout, fmt.Sprintf("self data: %d blobs of format 1 remain", n), "the repair with dana stopped")
	has(t, half.Stdout, "erin holds no piece now", "the repair with dana stopped")
	pieces := func(name string, want int) {
		t.Helper()
		entries := listAll(t, signer, name)
		for _, e := range entries {
			if len(e.tag) != storage.TagSize || storage.GoodSize(int64(len(e.data))) {
				t.Fatalf("%s holds %s of %d bytes with a tag of %d bytes; want a piece", name, e.id, len(e.data), len(e.tag))
			}
		}
		if len(entries) != want {
			t.Fatalf("%s holds %d pieces; want %d", name, len(entries), want)
		}
	}
	pieces("charlie", n)
	pieces("erin", 0)
	running["erin"].stop()
	readsAll("half", "with the move halfway, from charlie")
	running["erin"] = serve["erin"]()
	t.Log("ok: with dana stopped, charlie holds pieces only, erin holds nothing, and a new machine reads every page")

	// Dana comes back. The next repair puts a piece on dana, and deletes
	// its whole copies.
	running["dana"] = serve["dana"]()
	done := signer.try("", "sync", "--repair")
	t.Logf("arc sync --repair with dana back:\n%s\n%s", strings.TrimSpace(done.Stdout), strings.TrimSpace(done.Stderr))
	if done.Err != nil {
		t.Fatalf("arc sync --repair: %v\n%s\n%s", done.Err, done.Stdout, done.Stderr)
	}
	has(t, done.Stdout, fmt.Sprintf("coded %d blobs of format 1 into pieces", n), "the repair with dana back")
	if strings.Contains(done.Stdout, "blobs of format 1 remain") {
		t.Fatalf("blobs of format 1 remain after the repair with dana back:\n%s", done.Stdout)
	}
	pieces("charlie", n)
	pieces("dana", n)
	for _, name := range []string{"charlie", "dana"} {
		for _, path := range pieceFiles(t, filepath.Join(work, name+"-blobs")) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if storage.GoodSize(info.Size()) {
				t.Fatalf("%s keeps the file %s of %d bytes, a whole blob", name, path, info.Size())
			}
		}
	}
	running["dana"].stop()
	readsAll("from-charlie", "from charlie alone")
	running["dana"] = serve["dana"]()
	running["charlie"].stop()
	readsAll("from-dana", "from dana alone")
	running["charlie"] = serve["charlie"]()
	t.Log("ok: the next repair ended the move: charlie and dana hold pieces only, and a new machine reads every page from each alone")

	// The older arc cannot read a piece. Its sync fails, as section 7 of
	// the storage spec says.
	again := older.try("", "sync")
	t.Logf("the older arc after the move: %v\n%s\n%s", again.Err, strings.TrimSpace(again.Stdout), strings.TrimSpace(again.Stderr))
	if again.Err == nil {
		t.Fatal("the older arc synced with storage citizens that hold pieces only")
	}
	has(t, again.Stderr, "the storage citizen announced a blob of", "the older arc after the move")
}
