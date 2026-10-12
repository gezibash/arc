package proof

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestStorageNetworkSync proves storage network step 5 through the normal
// CLI, a real relay and three storage citizens. One sync writes a piece
// to each storage citizen at the same time, so it takes about the time of
// the slowest one, not the sum. With one storage citizen stopped, the sync
// still writes to the others, and a machine with only the seed reads every
// page from the pieces on them. After 7 days with no answer, each sync
// warns. A draining storage citizen is read, and gets no put. A gone one
// gets no sync.
func TestStorageNetworkSync(t *testing.T) {
	begin := time.Now()
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	signer := arc(t, filepath.Join(work, "signer")).with(loopback...)
	made := signer.try("", "keys", "gen")
	if made.Err != nil {
		t.Fatalf("arc keys gen: %v\n%s", made.Err, made.Stderr)
	}
	words := seedWords(made.Stderr)
	signer.run("relay", "add", url)
	citizen := signer.key()
	// Each call to a storage citizen waits on a slow link of its own.
	delays := map[string]time.Duration{"charlie": 400 * time.Millisecond, "dana": 800 * time.Millisecond, "erin": 1200 * time.Millisecond}
	charlie, charlieServe := servedStorageCitizen(t, work, url, signer, "charlie", delays["charlie"])
	dana, _ := servedStorageCitizen(t, work, url, signer, "dana", delays["dana"])
	erin, _ := servedStorageCitizen(t, work, url, signer, "erin", delays["erin"])
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")
	for name, key := range map[string]string{"charlie": charlie, "dana": dana, "erin": erin} {
		has(t, signer.run("keys", "storage", "add", key, "--as", name), "storage list: "+name+" is active", "the add of "+name)
	}

	pages := map[string]string{}
	writePage := func(n int, text string) {
		page := fmt.Sprintf("hrs/net/%d", n)
		pages[page] = text
		signer.input(fmt.Sprintf("---\ntitle: Page %d\npage: %d\nnotebook: hrs/net\n---\n%s\n", n, n, text), "journal", "write", page)
	}
	for n := 1; n <= 3; n++ {
		writePage(n, fmt.Sprintf("page %d of the network", n))
	}

	// One sync writes to each storage citizen at the same time. Thus the
	// sync takes about the time of the slowest one, not the sum.
	start := time.Now()
	first := signer.run("sync")
	whole := time.Since(start)
	var sum, slowest time.Duration
	for _, name := range []string{"charlie", "dana", "erin"} {
		got, took := syncTook(t, name, first)
		if got.sent == 0 {
			t.Fatalf("the sync put nothing on %s: %+v\n%s", name, got, first)
		}
		if took < delays[name] {
			t.Fatalf("%s took %s, less than the delay of its link, %s", name, took, delays[name])
		}
		t.Logf("measured: %s took %s with a delay of %s", name, took, delays[name])
		sum += took
		slowest = max(slowest, took)
	}
	t.Logf("measured: arc sync took %s; the storage citizens took %s together, the slowest %s", whole.Round(time.Millisecond), sum, slowest)
	// A sync one after the other takes at least the sum, so a time below
	// the sum proves that the syncs overlap. The start of the process adds
	// to the whole and not to the sum; a race build makes that start slow.
	if whole >= sum {
		t.Fatalf("arc sync took %s, and the storage citizens took %s together: they did not sync at the same time", whole, sum)
	}
	t.Log("ok: one sync writes a piece to three storage citizens at the same time")

	// With charlie stopped, the sync still writes to dana and erin.
	charlieServe.stop()
	writePage(4, "written while charlie is stopped")
	start = time.Now()
	stopped := signer.try("", "sync")
	t.Logf("measured: arc sync with charlie stopped took %s", time.Since(start).Round(time.Millisecond))
	if stopped.Err == nil {
		t.Fatalf("arc sync succeeded with charlie stopped; want an exit that names the failure\n%s", stopped.Stdout)
	}
	has(t, stopped.Stderr, "charlie: self data: ", "the sync with charlie stopped")
	t.Logf("charlie stopped: %s", strings.TrimSpace(stopped.Stderr))
	for _, name := range []string{"dana", "erin"} {
		if got, _ := syncTook(t, name, stopped.Stdout); got.sent == 0 {
			t.Fatalf("the sync with charlie stopped put nothing on %s: %+v", name, got)
		}
	}
	if strings.Contains(stopped.Stderr, "no answer for") {
		t.Fatalf("the first failed sync warns of silence:\n%s", stopped.Stderr)
	}
	t.Log("ok: with charlie stopped, the sync still writes to dana and erin")

	paper := arc(t, filepath.Join(work, "paper")).with(loopback...)
	recovered := ifaceTrim(paper.input(strings.Join(words, " ")+"\n", "keys", "recover"))
	if !strings.HasSuffix(recovered, citizen) {
		t.Fatalf("the seed made %q, not the citizen %s", recovered, citizen)
	}
	paper.run("relay", "add", url)
	paper.run("install", citizen, "journal", "--yes")
	fromSeed := paper.try("", "sync")
	has(t, fromSeed.Stderr, "charlie: self data: ", "the sync of the machine from the seed")
	for page, want := range pages {
		has(t, journalDoc(t, begin, paper.run("journal", "read", page)), want, "the seed reading "+page)
	}
	t.Logf("ok: a machine with only the seed reads all %d pages with charlie stopped", len(pages))

	// The device keeps the time of the first failed open. Seven days later
	// each sync warns. The proof moves that time back, as a clock would.
	state := selfState(t, signer.home, charlie)
	var record map[string]any
	if err := json.Unmarshal([]byte(read(t, state)), &record); err != nil {
		t.Fatal(err)
	}
	if record["silent_since"] == nil {
		t.Fatalf("the device kept no silent_since for charlie: %s", read(t, state))
	}
	record["silent_since"] = time.Now().Add(-8 * 24 * time.Hour).Unix()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	write(t, state, string(raw), 0o600)
	silent := signer.try("", "sync")
	has(t, silent.Stderr, "charlie: no answer for 8 days; if it is gone, run arc keys storage rm charlie --gone on the signer", "the sync after 8 days of silence")
	t.Log("ok: after 8 days with no answer, the sync warns, and names the command")

	// A draining storage citizen is read, and gets no put.
	has(t, signer.run("keys", "storage", "rm", "dana"), "storage list: dana is draining", "the drain of dana")
	danaBlobs, erinBlobs := blobFiles(t, filepath.Join(work, "dana-blobs")), blobFiles(t, filepath.Join(work, "erin-blobs"))
	writePage(5, "written while dana drains")
	drained := signer.try("", "sync")
	has(t, drained.Stdout, "; draining, so nothing new goes to it", "the sync of a draining storage citizen")
	if got, _ := syncTook(t, "dana", drained.Stdout); got.sent != 0 {
		t.Fatalf("the sync put %d blobs on dana, which drains", got.sent)
	}
	if n := blobFiles(t, filepath.Join(work, "dana-blobs")); n != danaBlobs {
		t.Fatalf("dana holds %d blobs after the drain, and %d before", n, danaBlobs)
	}
	if n := blobFiles(t, filepath.Join(work, "erin-blobs")); n <= erinBlobs {
		t.Fatalf("erin holds %d blobs after the sync, and %d before: the new page did not go to erin", n, erinBlobs)
	}
	// With charlie stopped, each blob of pages 1 to 4 needs its pieces on
	// dana and on erin: k is 2. A new machine thus reads from dana,
	// which drains.
	fromDana := arc(t, filepath.Join(work, "paper2")).with(loopback...)
	fromDana.input(strings.Join(words, " ")+"\n", "keys", "recover")
	fromDana.run("relay", "add", url)
	fromDana.run("install", citizen, "journal", "--yes")
	onlyDana := fromDana.try("", "sync")
	if got, _ := syncTook(t, "dana", onlyDana.Stdout); got.received == 0 {
		t.Fatalf("the sync read nothing from dana, which drains:\n%s\n%s", onlyDana.Stdout, onlyDana.Stderr)
	}
	for page, want := range pages {
		has(t, journalDoc(t, begin, fromDana.run("journal", "read", page)), want, "the reading of "+page+" with dana")
	}
	t.Log("ok: dana drains: a device reads its pieces, and puts nothing on it")

	// A gone storage citizen gets no sync.
	has(t, signer.run("keys", "storage", "rm", "dana", "--gone"), "storage list: dana is gone", "the removal of dana")
	gone := signer.try("", "sync")
	if regexp.MustCompile(`dana: self data`).MatchString(gone.Stdout + gone.Stderr) {
		t.Fatalf("the sync still syncs with dana after gone:\n%s\n%s", gone.Stdout, gone.Stderr)
	}
	has(t, gone.Stdout, "erin: self data received", "the sync after dana is gone")
	t.Log("ok: a gone storage citizen gets no sync")
}

// syncTook reads the report line of arc sync for one storage citizen, and
// the time that it took.
func syncTook(t *testing.T, name, output string) (selfCounts, time.Duration) {
	t.Helper()
	got := syncOf(t, name, output)
	m := regexp.MustCompile(regexp.QuoteMeta(name) + `: self data received \d+, sent \d+, already held \d+ in ([0-9.]+[a-zµ]+)`).FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("arc sync printed no time for %s:\n%s", name, output)
	}
	took, err := time.ParseDuration(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return got, took
}

// selfState finds the sync state of one storage citizen under home.
func selfState(t *testing.T, home, key string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == key+".json" && filepath.Base(filepath.Dir(path)) == "selfsync" {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no sync state of %s under %s", key, home)
	}
	return found
}

// blobFiles counts the blob files of each account under a storage
// directory.
func blobFiles(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "accounts", "*", "blobs", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}
