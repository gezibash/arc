package proof

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	keyfiles "github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/draft"
)

// The two ends of each pipe are on one machine, so they use the loopback
// address and no STUN server.
var loopback = []string{"ARC_STUN=none", "ARC_LOOPBACK=1"}

// storageCitizen serves the storage app for the owner with the key owner,
// and returns the key of the storage citizen.
func storageCitizen(t *testing.T, work, url, owner string) string {
	t.Helper()
	store := arc(t, filepath.Join(work, "storage")).with("STORAGE_DIR="+filepath.Join(work, "blobs"),
		"STORAGE_OWNERS="+owner, "STORAGE_STUN=none", "STORAGE_LOOPBACK=1")
	store.run("keys", "gen")
	store.run("relay", "add", url)
	app := filepath.Join(work, "storage-app")
	write(t, filepath.Join(app, "Arcfile"), fmt.Sprintf("version = 2\n\n[serve]\ncommand = %q\nmanifest = %q\n",
		program("arc-storage"), filepath.Join(repo, "apps/storage/manifest.json")), 0o600)
	if p := store.serve(app); !p.waitFor("serves storage", 10*time.Second) {
		t.Fatalf("the storage citizen did not serve:\n%s", p.output())
	}
	return store.key()
}

// relaySelfData returns the self data of the citizen of home that the relay
// holds: drafts, checkpoints, parts and their deletions. It leaves out the
// draft of the keyed root, which stays on the relay.
func relaySelfData(t *testing.T, url, home string) []nostr.Event {
	t.Helper()
	key, err := keyfiles.Load(keyfile(t, home))
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(url, keyer.NewPlainKeySigner(key.Secret))
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	batch, err := r.Fetch(ctx, nostr.Filter{
		Kinds:   []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, nostr.KindDeletion},
		Authors: []nostr.PubKey{key.Public},
	})
	if err != nil {
		t.Fatalf("the relay did not answer: %v", err)
	}
	var out []nostr.Event
	for _, event := range batch.Events {
		if event.Kind == draft.Kind && event.Tags.GetD() == "arc-keyed-root" {
			continue
		}
		out = append(out, event)
	}
	return out
}

type selfCounts struct{ received, sent, held int }

var selfLine = regexp.MustCompile(`storage: self data received (\d+), sent (\d+), already held (\d+)`)

// selfSync reads the report line of arc sync for the storage citizen.
func selfSync(t *testing.T, output string) selfCounts {
	t.Helper()
	m := selfLine.FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("arc sync printed no report of the storage citizen:\n%s", output)
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return selfCounts{n(m[1]), n(m[2]), n(m[3])}
}

// TestStorageJournalSync proves storage step 2 through the normal CLI, a
// local relay and a storage citizen. The journal is a local app: a page that
// one machine writes reaches a second machine of the same citizen through
// the storage citizen, and no draft of it goes to the relay.
func TestStorageJournalSync(t *testing.T) {
	begin := time.Now()
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	laptop := arc(t, filepath.Join(work, "laptop")).with(loopback...)
	desktop := arc(t, filepath.Join(work, "desktop")).with(loopback...)
	laptop.run("keys", "gen")
	desktop.input(read(t, keyfile(t, laptop.home)), "keys", "add")
	laptop.run("relay", "add", url)
	desktop.run("relay", "add", url)
	citizen := laptop.key()

	storeKey := storageCitizen(t, work, url, citizen)
	laptop.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	for _, one := range []machine{laptop, desktop} {
		one.run("install", citizen, "journal", "--yes")
		one.run("install", storeKey, "storage", "--yes")
	}
	t.Log("ok: two machines of one citizen install the journal and one storage citizen")

	laptop.input("---\ntitle: LR sweep\npage: 1\nnotebook: hrs/ablations\n---\nauc 0.871\n",
		"journal", "write", "hrs/ablations/1")
	laptop.run("journal", "append", "hrs/ablations/1", "next:", "try", "warmup")
	if got := relaySelfData(t, url, laptop.home); len(got) != 0 {
		t.Fatalf("the relay holds %d events of the journal after a write, want 0: kind %d", len(got), got[0].Kind)
	}
	if got := desktop.try("", "journal", "read", "hrs/ablations/1"); got.Stdout != "" {
		t.Fatalf("the desktop read the page before a sync:\n%s", got.Stdout)
	}
	t.Log("ok: a write stays on the laptop, and the relay holds no event of it")

	up := selfSync(t, laptop.run("sync"))
	if up.sent == 0 {
		t.Fatalf("the laptop put nothing on the storage citizen: %+v", up)
	}
	down := selfSync(t, desktop.run("sync"))
	if down.received == 0 {
		t.Fatalf("the desktop got nothing from the storage citizen: %+v", down)
	}
	want := "---\ntitle: LR sweep\npage: 1\nnotebook: hrs/ablations\n---\nauc 0.871\nnext: try warmup\n"
	if got := journalDoc(t, begin, desktop.run("journal", "read", "hrs/ablations/1")); got != want {
		t.Fatalf("the desktop read\n%s\nwant\n%s", got, want)
	}
	t.Logf("ok: the page crosses the storage citizen: the laptop put %d blobs, the desktop took %d records", up.sent, down.received)

	// The other way, and a second sync takes only the change.
	desktop.run("journal", "append", "hrs/ablations/1", "from", "the", "desktop")
	desktop.run("sync")
	again := selfSync(t, laptop.run("sync"))
	if again.received == 0 || again.held != 0 {
		t.Fatalf("the second sync of the laptop: %+v, want new records and none held", again)
	}
	if got := laptop.run("journal", "read", "hrs/ablations/1"); !strings.Contains(got, "from the desktop") {
		t.Fatalf("the laptop did not read the append of the desktop:\n%s", got)
	}
	if got := relaySelfData(t, url, laptop.home); len(got) != 0 {
		t.Fatalf("the relay holds %d events of the journal after the syncs, want 0", len(got))
	}
	t.Log("ok: a change crosses back, a second sync gets only the change, and the relay still holds no journal event")

	// Each blob on the storage citizen has a padded size.
	err := filepath.WalkDir(filepath.Join(work, "blobs"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(filepath.Dir(path)) != "blobs" {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if size := info.Size(); size < 4096 || size&(size-1) != 0 {
			return fmt.Errorf("%s has %d bytes, which is not a power of two from 4 KiB", path, size)
		}
		if strings.Contains(read(t, path), "warmup") {
			return fmt.Errorf("%s holds the text of the page", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ok: each blob is padded to a power of two, and holds no text of the page")

	// A phone signs through the bunker of the laptop. The device key of
	// the phone signs its changes. The laptop and the desktop hold the
	// citizen key, and sign with it.
	const limit = 10 * time.Second
	phone := arc(t, filepath.Join(work, "phone")).with(loopback...)
	phoneDevice := ifaceTrim(phone.run("keys", "device"))
	// The reply of a live call is sealed to the citizen, so the phone
	// needs the bunker to open it.
	bunker := laptop.start("keys", "bunker", "--relay", url, "--decrypt", "all")
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	laptop.run("keys", "devices", "add", phoneDevice, "--name", "phone")
	if !loads(bunker, 1, limit) {
		t.Fatalf("the bunker did not load the device list:\n%s", bunker.output())
	}
	phone.run("keys", "add", "bunker://"+uri)
	phone.run("relay", "add", url)
	phone.run("install", citizen, "journal", "--yes")
	phone.run("install", storeKey, "storage", "--yes")
	phone.run("sync")
	if got := phone.run("journal", "read", "hrs/ablations/1"); !strings.Contains(got, "from the desktop") {
		t.Fatalf("the phone did not read the page after a sync:\n%s", got)
	}
	t.Log("ok: a device that signs through the bunker reads the page after a sync")

	// With a device list, a machine that holds the citizen key is still
	// taken: it signs with the citizen key.
	laptop.run("journal", "append", "hrs/ablations/1", "laptop", "with", "a", "list")
	laptop.run("sync")
	desktop.run("sync")
	if got := desktop.run("journal", "read", "hrs/ablations/1"); !strings.Contains(got, "laptop with a list") {
		t.Fatalf("the desktop did not take a change of the laptop, which holds the citizen key:\n%s", got)
	}
	t.Log("ok: with a device list, the changes of the machines that hold the citizen key cross")

	// The phone writes, and is revoked before the laptop syncs. The laptop
	// refuses the change. After the phone joins the list again, the next
	// sync takes it.
	phone.run("journal", "append", "hrs/ablations/1", "from", "the", "phone")
	phone.run("sync")
	laptop.run("keys", "devices", "revoke", "phone")
	refused := laptop.try("", "sync")
	if !strings.Contains(refused.Stderr, "not on the device list: "+phoneDevice) {
		t.Fatalf("the laptop took a change of a revoked device:\nstdout: %s\nstderr: %s", refused.Stdout, refused.Stderr)
	}
	if got := laptop.run("journal", "read", "hrs/ablations/1"); strings.Contains(got, "from the phone") {
		t.Fatalf("the laptop reads the change of the revoked phone:\n%s", got)
	}
	laptop.run("keys", "devices", "add", phoneDevice, "--name", "phone")
	if got := selfSync(t, laptop.run("sync")); got.received == 0 {
		t.Fatalf("the laptop did not take the change after the phone joined the list again: %+v", got)
	}
	if got := laptop.run("journal", "read", "hrs/ablations/1"); !strings.Contains(got, "from the phone") {
		t.Fatalf("the laptop did not read the change of the listed phone:\n%s", got)
	}
	t.Log("ok: the device key signs each change, and a change of a device that is not on the list is refused")
}

// TestStorageImportsDrafts proves the one-time import: the drafts that an
// older client put on the relay move to the local store and to the storage
// citizen. A second import changes nothing.
func TestStorageImportsDrafts(t *testing.T) {
	begin := time.Now()
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	old := arc(t, filepath.Join(work, "old")).with(loopback...)
	old.run("keys", "gen")
	old.run("relay", "add", url)
	citizen := old.key()
	old.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	old.run("install", citizen, "journal", "--yes")

	// An older client sent each draft to the relay. Here one machine
	// writes two pages, and the test sends the events to the relay as
	// that client did.
	old.input("---\ntitle: One\npage: 1\nnotebook: hrs/old\n---\nfirst page\n", "journal", "write", "hrs/old/1")
	old.input("---\ntitle: Two\npage: 2\nnotebook: hrs/old\n---\nsecond page\n", "journal", "write", "hrs/old/2")
	stick := filepath.Join(work, "stick")
	old.run("sync", "--dir", stick)
	key, err := keyfiles.Load(keyfile(t, old.home))
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(url, keyer.NewPlainKeySigner(key.Secret))
	files, err := filepath.Glob(filepath.Join(stick, "events", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the stick holds no events: %v", err)
	}
	sent := 0
	for _, path := range files {
		var event nostr.Event
		if err := json.Unmarshal([]byte(read(t, path)), &event); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if event.PubKey != key.Public || event.Kind == nostr.KindDeletion {
			continue
		}
		if err := r.Send(context.Background(), event); err != nil {
			t.Fatalf("the relay refused a draft: %v", err)
		}
		sent++
	}
	r.Close()
	onRelay := len(relaySelfData(t, url, old.home))
	if onRelay == 0 {
		t.Fatalf("the relay holds no draft of the older client (sent %d)", sent)
	}
	t.Logf("ok: the relay holds %d drafts, checkpoints and parts of an older client", onRelay)

	// A new machine of the citizen, with a storage citizen.
	fresh := arc(t, filepath.Join(work, "fresh")).with(loopback...)
	fresh.input(read(t, keyfile(t, old.home)), "keys", "add")
	fresh.run("relay", "add", url)
	storeKey := storageCitizen(t, work, url, citizen)
	fresh.run("install", citizen, "journal", "--yes")
	fresh.run("install", storeKey, "storage", "--yes")

	first := fresh.run("sync")
	imported := regexp.MustCompile(`imported (\d+) drafts`).FindStringSubmatch(first)
	if imported == nil {
		t.Fatalf("the first sync imported nothing:\n%s", first)
	}
	// The draft of the keyed root comes too, so the count can be one more.
	if n, _ := strconv.Atoi(imported[1]); n < onRelay {
		t.Fatalf("the first sync did not import the %d drafts of the relay:\n%s", onRelay, first)
	}
	put := selfSync(t, first)
	if put.sent < onRelay {
		t.Fatalf("the storage citizen got %d blobs, want at least the %d imported drafts", put.sent, onRelay)
	}
	if got := journalDoc(t, begin, fresh.run("journal", "read", "hrs/old/2")); !strings.Contains(got, "second page") {
		t.Fatalf("the new machine did not read the imported page:\n%s", got)
	}
	t.Logf("ok: the first sync imports %d drafts, and puts them on the storage citizen", onRelay)

	second := fresh.run("sync", "--import")
	if strings.Contains(second, "imported") {
		t.Fatalf("a second import took drafts again:\n%s", second)
	}
	if got := selfSync(t, second); got.sent != 0 || got.received != 0 {
		t.Fatalf("a second import changed the storage citizen: %+v", got)
	}
	if ls := fresh.run("journal", "ls"); strings.Count(ls, "hrs/old/") != 2 {
		t.Fatalf("the journal lists %d pages after two imports, want 2:\n%s", strings.Count(ls, "hrs/old/"), ls)
	}
	if left := len(relaySelfData(t, url, old.home)); left != onRelay {
		t.Fatalf("the relay holds %d drafts after the import, want the same %d: they stay for older clients", left, onRelay)
	}
	t.Log("ok: a second import changes nothing, and the drafts stay on the relay for older clients")
}
