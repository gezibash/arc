package proof

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/storage"
)

// TestStorageRepair proves storage network step 7 through the normal CLI,
// a real relay and five storage citizens:
//
//   - The audit of arc sync --repair finds changed pieces. Repair seals
//     again a record with 2 of its 3 pieces changed (rule 2) and one with 1
//     changed piece, and a new machine reads each page from 2 storage
//     citizens.
//   - A draining storage citizen gives each piece, with the same bytes, to
//     a new one, and holds nothing after. The sync of the signer then
//     marks it gone in the storage list, with no rm --gone.
//   - A storage citizen that is stopped for good and marked gone: arc serve
//     rebuilds its pieces on a new storage citizen, and a new machine reads
//     each page without it.
func TestStorageRepair(t *testing.T) {
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
	serve := map[string]func() *process{}
	running := map[string]*process{}
	keys := map[string]string{}
	add := func(name string) {
		t.Helper()
		keys[name], serve[name] = storageServer(t, work, url, signer, name, program("arc-storage"), 0)
		running[name] = serve[name]()
		has(t, signer.run("keys", "storage", "add", keys[name], "--as", name), "storage list: "+name+" is active", "the add of "+name)
	}
	for _, name := range []string{"charlie", "dana", "erin"} {
		add(name)
	}
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")
	pages := map[string]string{}
	for n := 1; n <= 4; n++ {
		page := fmt.Sprintf("hrs/fix/%d", n)
		pages[page] = fmt.Sprintf("the text of page %d to repair", n)
		signer.input(fmt.Sprintf("---\ntitle: Page %d\npage: %d\nnotebook: hrs/fix\n---\n%s\n", n, n, pages[page]), "journal", "write", page)
	}
	first := signer.try("", "sync")
	if first.Err != nil {
		t.Fatalf("arc sync: %v\n%s\n%s", first.Err, first.Stdout, first.Stderr)
	}
	has(t, first.Stdout, "2 of 3 rebuild each", "the first sync")
	readsAll := func(name string, what string) {
		t.Helper()
		m := fromSeed(t, work, name, url, citizen, words)
		got := m.try("", "sync")
		for page, want := range pages {
			has(t, journalDoc(t, begin, m.run("journal", "read", page)), want, "the reading of "+page+" "+what+"\n"+got.Stdout+got.Stderr)
		}
	}

	// The pieces of one blob have the same place in the listing of each
	// storage citizen: the device puts them in the order of the records.
	// One byte changes in the first piece of charlie and of dana, so the
	// first blob has 1 good piece of 3, and in the second piece of dana.
	change := func(name string, n int) {
		t.Helper()
		entries := listAll(t, signer, name)
		path := filepath.Join(work, name+"-blobs")
		for _, file := range pieceFiles(t, path) {
			if filepath.Base(file) == entries[n].id {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)/2] ^= 1
				write(t, file, string(data), 0o600)
				return
			}
		}
		t.Fatalf("%s has no file for the piece %s", name, entries[n].id)
	}
	n := len(listAll(t, signer, "charlie"))
	change("charlie", 0)
	change("dana", 0)
	change("dana", 1)
	plain := signer.try("", "sync")
	if strings.Contains(plain.Stdout, "audit checked") {
		t.Fatalf("arc sync with no --repair audited:\n%s", plain.Stdout)
	}
	fixed := signer.try("", "sync", "--repair")
	t.Logf("arc sync --repair:\n%s\n%s", strings.TrimSpace(fixed.Stdout), strings.TrimSpace(fixed.Stderr))
	has(t, fixed.Stdout, fmt.Sprintf("charlie: audit checked %d pieces, 1 failed", n), "the audit of charlie")
	has(t, fixed.Stdout, fmt.Sprintf("dana: audit checked %d pieces, 2 failed", n), "the audit of dana")
	has(t, fixed.Stdout, fmt.Sprintf("erin: audit checked %d pieces, 0 failed", n), "the audit of erin")
	has(t, fixed.Stdout, "sealed 2 records again", "the repair")
	running["erin"].stop()
	readsAll("after-audit", "from charlie and dana after the repair")
	running["erin"] = serve["erin"]()
	t.Log("ok: the audit found 3 changed pieces; repair sealed the 2 records again, and a new machine reads each page from charlie and dana")

	// A fourth storage citizen joins, and erin drains. A sync with no
	// --repair moves nothing, and says that blobs need repair.
	add("frank")
	before := listAll(t, signer, "erin")
	has(t, signer.run("keys", "storage", "rm", "erin"), "draining", "the rm of erin")
	waiting := signer.try("", "sync")
	has(t, waiting.Stderr, "blobs need repair; arc serve repairs them on its tick, or run arc sync --repair", "a sync with erin draining")
	if n := len(listAll(t, signer, "frank")); n != 0 {
		t.Fatalf("a sync with no --repair put %d pieces on frank", n)
	}
	drained := signer.try("", "sync", "--repair")
	t.Logf("arc sync --repair with erin draining:\n%s\n%s", strings.TrimSpace(drained.Stdout), strings.TrimSpace(drained.Stderr))
	has(t, drained.Stdout, fmt.Sprintf("repair rebuilt 0 pieces, moved %d,", n), "the drain")
	has(t, drained.Stdout, "erin holds no piece now and takes no put, so the storage list marks it gone", "the drain")
	has(t, drained.Stdout, "storage list: erin is gone", "the end of the drain")
	has(t, drained.Stdout, "erin: the account is closed, and the storage citizen removed it", "the close of erin")
	has(t, signer.run("keys", "storage", "ls"), "erin\tgone\t"+keys["erin"], "the storage list after the drain")
	erinAccount := ifaceTrim(signer.run("keys", "account", keys["erin"]))
	if _, err := os.Stat(filepath.Join(work, "erin-blobs", "accounts", erinAccount)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory of the closed account on erin: %v; want it removed", err)
	}
	t.Log("ok: the signer closed the empty account on erin, and erin removed its directory")
	// erin admits the account key by STORAGE_OWNERS, so a session opens a
	// new, empty account.
	if left := listAll(t, signer, "erin"); len(left) != 0 {
		t.Fatalf("erin still lists %d pieces after the drain", len(left))
	}
	after := listAll(t, signer, "frank")
	for _, piece := range before {
		found := false
		for _, got := range after {
			found = found || bytes.Equal(got.data, piece.data)
		}
		if !found {
			t.Fatalf("the piece %s of erin did not move to frank with the same bytes", piece.id)
		}
	}
	running["erin"].stop()
	readsAll("after-drain", "from charlie, dana and frank after the drain, with erin stopped")
	t.Log("ok: the drain moved each piece of erin to frank with the same bytes, erin holds nothing, the sync of the signer marked it gone, and a new machine reads each page without erin")

	// Charlie stops for good, and its directory goes. A fifth storage
	// citizen joins, and the signer marks charlie gone. arc serve rebuilds
	// the pieces of charlie on gina at its first tick.
	add("gina")
	running["charlie"].stop()
	if err := os.RemoveAll(filepath.Join(work, "charlie-blobs")); err != nil {
		t.Fatal(err)
	}
	has(t, signer.run("keys", "storage", "rm", "charlie", "--gone"), "gone", "the rm of charlie --gone")
	app := filepath.Join(work, "signer-app")
	write(t, filepath.Join(app, "Arcfile"), fmt.Sprintf("version = 2\n\n[serve]\ncommand = %q\nmanifest = %q\n",
		program("arc-storage"), filepath.Join(repo, "apps/storage/manifest.json")), 0o600)
	served := signer.with("STORAGE_DIR="+filepath.Join(work, "signer-blobs"), "STORAGE_STUN=none", "STORAGE_LOOPBACK=1").serve(app)
	if !served.waitFor("self data: repair rebuilt", 60*time.Second) {
		t.Fatalf("arc serve did not repair:\n%s", served.output())
	}
	served.stop()
	repaired := served.output()
	t.Logf("arc serve:\n%s", repaired)
	has(t, repaired, fmt.Sprintf("self data: repair rebuilt %d pieces", n), "the repair of arc serve")
	if got := len(listAll(t, signer, "gina")); got != n {
		t.Fatalf("gina holds %d pieces after the repair; want %d", got, n)
	}
	running["dana"].stop()
	readsAll("after-gone", "from frank and gina, with charlie gone and dana stopped")
	t.Log("ok: arc serve rebuilt each piece of charlie on gina, and a new machine reads each page from frank and gina")

	// The close is not a one-way door: the signer adds erin again, and a
	// sync puts the data back on erin. The usage line of each storage
	// citizen gives the bytes of its files.
	running["erin"] = serve["erin"]()
	has(t, signer.run("keys", "storage", "add", keys["erin"], "--as", "erin"), "storage list: erin is active", "the add of erin after its close")
	back := signer.try("", "sync", "--repair")
	t.Logf("arc sync --repair after the add of erin:\n%s\n%s", strings.TrimSpace(back.Stdout), strings.TrimSpace(back.Stderr))
	if got := len(listAll(t, signer, "erin")); got == 0 {
		t.Fatalf("erin holds no piece after its add and a sync")
	}
	for _, name := range []string{"erin", "frank", "gina"} {
		var used int64
		files := pieceFiles(t, filepath.Join(work, name+"-blobs"))
		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			used += info.Size()
		}
		want := fmt.Sprintf("%s: %s of 1 GiB in use, %d blobs, plan owner, open", name, storage.HumanSize(used), len(files))
		has(t, back.Stdout, want, "the usage line of "+name)
	}
	running["frank"].stop()
	readsAll("after-reopen", "from erin and gina after the add of erin, with frank stopped")
	t.Log("ok: after the close, arc keys storage add opened a new account on erin, the sync put the data back, and the usage line of each storage citizen gives the bytes of its files")
}

// TestStorageRepairTURN proves decision 7 of the storage network proposal
// through the CLI and a relay that serves TURN: with arc path turn, each
// pipe to a storage citizen goes through TURN, and arc sync --repair does
// not repair, and says why. With --turn it repairs.
func TestStorageRepairTURN(t *testing.T) {
	work := t.TempDir()
	relayHome := arc(t, filepath.Join(work, "relay")).with("TURN_SECRET=proof-turn-secret")
	p := relayHome.start("relay", "serve", "--listen", "127.0.0.1:0", "--stun", "127.0.0.1:0", "--turn-ports", "50240-50279", "--turn-ip", "127.0.0.1")
	if !p.waitFor("relay listens on ", 10*time.Second) {
		t.Fatalf("the relay did not start:\n%s", p.output())
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the relay:\n%s", p.output())
		}
	})
	var url string
	for text := range strings.SplitSeq(p.output(), "\n") {
		if u, ok := strings.CutPrefix(text, "relay listens on "); ok {
			url = strings.TrimSpace(u)
		}
	}

	signer := arc(t, filepath.Join(work, "signer")).with(loopback...)
	signer.run("keys", "gen")
	signer.run("relay", "add", url)
	citizen := signer.key()
	for _, name := range []string{"charlie", "dana", "erin"} {
		key, start := storageServer(t, work, url, signer, name, program("arc-storage"), 0)
		start()
		signer.run("keys", "storage", "add", key, "--as", name)
	}
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")
	signer.input("---\ntitle: Page\npage: 1\nnotebook: hrs/turn\n---\nthe text of a page through TURN\n", "journal", "write", "hrs/turn/1")
	if r := signer.try("", "sync"); r.Err != nil {
		t.Fatalf("arc sync: %v\n%s\n%s", r.Err, r.Stdout, r.Stderr)
	}
	n := len(listAll(t, signer, "charlie"))
	key, start := storageServer(t, work, url, signer, "frank", program("arc-storage"), 0)
	start()
	signer.run("keys", "storage", "add", key, "--as", "frank")

	signer.run("path", "turn")
	waits := signer.try("", "sync", "--repair")
	t.Logf("arc sync --repair through TURN:\n%s\n%s", strings.TrimSpace(waits.Stdout), strings.TrimSpace(waits.Stderr))
	has(t, waits.Stderr, "waits for a direct path: the pipe to charlie, dana, erin, frank goes through TURN; arc sync --repair --turn allows TURN", "a repair through TURN")
	has(t, waits.Stdout, "repair rebuilt 0 pieces", "a repair through TURN")
	signer.run("path", "auto")
	if got := len(listAll(t, signer, "frank")); got != 0 {
		t.Fatalf("a repair through TURN put %d pieces on frank", got)
	}
	signer.run("path", "turn")
	turned := signer.try("", "sync", "--repair", "--turn")
	t.Logf("arc sync --repair --turn:\n%s\n%s", strings.TrimSpace(turned.Stdout), strings.TrimSpace(turned.Stderr))
	has(t, turned.Stdout, fmt.Sprintf("repair rebuilt %d pieces", n), "a repair with --turn")
	signer.run("path", "auto")
	if got := len(listAll(t, signer, "frank")); got != n {
		t.Fatalf("a repair with --turn put %d pieces on frank; want %d", got, n)
	}
	refused := signer.try("", "sync", "--turn")
	if refused.Err == nil || !strings.Contains(refused.Stderr, "give it with --repair") {
		t.Fatalf("arc sync --turn with no --repair did not fail:\n%s", refused.Stderr)
	}
	t.Log("ok: repair waits for a direct path, and says so; with --turn it rebuilds the pieces through TURN")
}
