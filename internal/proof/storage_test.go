package proof

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/sdk/storage"
)

// The two ends of each pipe are on one machine, so they use the loopback
// address and no STUN server.
var loopback = []string{"ARC_STUN=none", "ARC_LOOPBACK=1"}

// storageCitizen serves the storage app for the citizen of signer, and
// returns the key of the storage citizen. The owner of the account is the
// account key that the signer derives for this storage citizen.
func storageCitizen(t *testing.T, work, url string, signer machine) string {
	t.Helper()
	return namedStorageCitizen(t, work, url, signer, "storage")
}

// namedStorageCitizen is storageCitizen with its own directories under work.
func namedStorageCitizen(t *testing.T, work, url string, signer machine, name string) string {
	t.Helper()
	key, _ := servedStorageCitizen(t, work, url, signer, name, 0)
	return key
}

// servedStorageCitizen is namedStorageCitizen that also returns the process
// of arc serve, so that a test can stop the storage citizen. With a delay,
// slowcall holds each call to the storage app for that time.
func servedStorageCitizen(t *testing.T, work, url string, signer machine, name string, delay time.Duration) (string, *process) {
	t.Helper()
	key, serve := storageServer(t, work, url, signer, name, program("arc-storage"), delay)
	return key, serve()
}

// storageServer makes a storage citizen that serves the storage program
// app, and returns its key and a function that starts its arc serve. A
// test can thus stop a storage citizen, and start it again.
func storageServer(t *testing.T, work, url string, signer machine, name, app string, delay time.Duration) (string, func() *process) {
	t.Helper()
	store := arc(t, filepath.Join(work, name))
	store.run("keys", "gen")
	key := store.key()
	account := ifaceTrim(signer.run("keys", "account", key))
	store = store.with("STORAGE_DIR="+filepath.Join(work, name+"-blobs"),
		"STORAGE_OWNERS="+account, "STORAGE_STUN=none", "STORAGE_LOOPBACK=1")
	command := app
	if delay > 0 {
		command = program("slowcall")
		store = store.with("SLOWCALL_PROGRAM="+app, fmt.Sprintf("SLOWCALL_MS=%d", delay.Milliseconds()),
			"SLOWCALL_LOG="+holdLog(work, name))
	}
	store.run("relay", "add", url)
	dir := filepath.Join(work, name+"-app")
	write(t, filepath.Join(dir, "Arcfile"), fmt.Sprintf("version = 2\n\n[serve]\ncommand = %q\nmanifest = %q\n",
		command, filepath.Join(repo, "apps/storage/manifest.json")), 0o600)
	return key, func() *process {
		t.Helper()
		p := store.serve(dir)
		if !p.waitFor("serves storage", 10*time.Second) {
			t.Fatalf("the storage citizen did not serve:\n%s", p.output())
		}
		return p
	}
}

// relaySelfData returns the self data of the citizen of home that the relay
// holds: drafts, checkpoints, parts and their deletions. It leaves out the
// draft of the keyed root, which stays on the relay, and the deletions that
// a device sent for the drafts that it moved to a storage citizen.
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
		if event.Kind == draft.Kind && event.Tags.GetD() == "arc-keyed-root" || draft.IsMoved(event) {
			continue
		}
		out = append(out, event)
	}
	return out
}

type selfCounts struct{ received, sent, held int }

var selfLine = regexp.MustCompile(`storage: self data received (\d+), sent (\d+), already held (\d+)`)

var tookLine = regexp.MustCompile(`self data: took (\d+) records`)

// took reads the count of records that arc sync took from all storage
// citizens.
func took(t *testing.T, output string) int {
	t.Helper()
	m := tookLine.FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("arc sync printed no line of the whole sync:\n%s", output)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

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

	storeKey := storageCitizen(t, work, url, laptop)
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
	err := filepath.WalkDir(filepath.Join(work, "storage-blobs"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(filepath.Dir(path)) != "blobs" {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if size := info.Size(); !storage.GoodPieceSize(size) {
			return fmt.Errorf("%s has %d bytes, which is not the size of a piece of a blob of a power of two from 4 KiB", path, size)
		}
		if strings.Contains(read(t, path), "warmup") {
			return fmt.Errorf("%s holds the text of the page", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ok: each piece is a piece of a blob padded to a power of two, and holds no text of the page")

	// A phone signs through the bunker of the laptop. The bunker keeps its
	// default decrypt policy. The phone signs its storage calls with its
	// device key, so the storage citizen seals each reply to the device
	// key. The device key of the phone also signs its changes. The laptop
	// and the desktop hold the citizen key: they sign storage calls with
	// the account key, and their changes with the citizen key.
	const limit = 10 * time.Second
	phone := arc(t, filepath.Join(work, "phone")).with(loopback...)
	phoneDevice := ifaceTrim(phone.run("keys", "device"))
	phoneKey, err := keyfiles.Load(filepath.Join(phone.home, "device"))
	if err != nil {
		t.Fatal(err)
	}
	bunker := laptop.start("keys", "bunker", "--relay", url)
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	if code := openStorage(t, url, phoneKey, storeKey); code != "not_owner" {
		t.Fatalf("the storage citizen answered a device with no grant with %q, want not_owner", code)
	}
	if added := laptop.run("keys", "devices", "add", phoneDevice, "--name", "phone"); !strings.Contains(added, "grants: sent to the storage citizen storage") {
		t.Fatalf("the signer did not send the grants to the storage citizen:\n%s", added)
	}
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
	t.Log("ok: a device behind a bunker with the default decrypt policy reads the page after a sync")

	// With a device list, a machine that holds the citizen key is still
	// taken: it signs with the citizen key.
	laptop.run("journal", "append", "hrs/ablations/1", "laptop", "with", "a", "list")
	laptop.run("sync")
	desktop.run("sync")
	if got := desktop.run("journal", "read", "hrs/ablations/1"); !strings.Contains(got, "laptop with a list") {
		t.Fatalf("the desktop did not take a change of the laptop, which holds the citizen key:\n%s", got)
	}
	t.Log("ok: with a device list, the changes of the machines that hold the citizen key cross")

	// A grant names one storage citizen. The phone has a grant on the first
	// storage citizen, and none on a second, which the signer has not
	// installed. After the signer installs it and syncs, the second takes
	// the phone too.
	otherKey := namedStorageCitizen(t, work, url, laptop, "other")
	phone.run("install", otherKey, "storage", "--as", "other", "--yes")
	other := phone.try("", "sync")
	if other.Err == nil || !strings.Contains(other.Stderr, "other: self data: the storage citizen refused this machine") {
		t.Fatalf("a storage citizen with no grant of the phone took it:\nstdout: %s\nstderr: %s", other.Stdout, other.Stderr)
	}
	if code := openStorage(t, url, phoneKey, otherKey); code != "not_owner" {
		t.Fatalf("the second storage citizen answered the phone with %q, want not_owner", code)
	}
	laptop.run("install", otherKey, "storage", "--as", "other", "--yes")
	laptop.run("sync")
	phone.run("sync")
	t.Log("ok: a device with a grant for another storage citizen is refused, until the signer sends a grant for this one")

	// The phone writes, and is revoked before the laptop syncs. The signer
	// sends the new grant set at once. The storage citizen then refuses the
	// device key itself, so a thief who holds the device key and skips the
	// bunker gets no session. The change of the phone is older than the
	// revocation, and comes from the storage citizen, so the laptop takes
	// it. The revoked key cannot join the list again.
	phone.run("journal", "append", "hrs/ablations/1", "from", "the", "phone")
	phone.run("sync")
	if code := openStorage(t, url, phoneKey, storeKey); code != "invalid_request" {
		t.Fatalf("the storage citizen answered the granted phone with %q, want invalid_request for the fake offer", code)
	}
	// A change of the second of the revocation counts as after it.
	time.Sleep(time.Second)
	if revoked := laptop.run("keys", "devices", "revoke", "phone"); !strings.Contains(revoked, "grants: sent to the storage citizen storage") {
		t.Fatalf("the signer did not send the revocation to the storage citizen:\n%s", revoked)
	}
	for _, key := range []string{storeKey, otherKey} {
		if code := openStorage(t, url, phoneKey, key); code != "not_owner" {
			t.Fatalf("a storage citizen answered the revoked phone with %q, want not_owner", code)
		}
	}
	t.Log("ok: a revoked device gets no session from either storage citizen")
	// With two storage citizens, k is 1, and the one piece may come from
	// either. The line of the whole sync counts the records.
	if synced := laptop.try("", "sync"); synced.Err != nil || took(t, synced.Stdout) == 0 {
		t.Fatalf("the laptop did not take the change that the phone made before its revocation: %v\n%s\n%s", synced.Err, synced.Stdout, synced.Stderr)
	}
	if got := laptop.run("journal", "read", "hrs/ablations/1"); !strings.Contains(got, "from the phone") {
		t.Fatalf("the laptop did not read the change that the phone made before its revocation:\n%s", got)
	}
	has(t, laptop.refuse("keys", "devices", "add", phoneDevice, "--name", "phone"), "the device key was revoked", "a revoked key joining the list again")
	t.Log("ok: the device key signs each change, a change from before the revocation stays readable, and a revoked key cannot join again")

	// Neither storage citizen learns the citizen key. Each account is named
	// by its account key, and no file of a storage citizen holds the
	// citizen key.
	for _, c := range []struct{ name, key string }{{"storage", storeKey}, {"other", otherKey}} {
		account := ifaceTrim(laptop.run("keys", "account", c.key))
		entries, err := os.ReadDir(filepath.Join(work, c.name+"-blobs", "accounts"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != account {
			t.Fatalf("the accounts of %s are %v, want only the account key %s", c.name, entries, account)
		}
		state := read(t, filepath.Join(work, c.name+"-blobs", "accounts", account, "state.json"))
		if strings.Contains(state, phoneDevice) {
			t.Fatalf("the account of %s holds a grant of the revoked phone:\n%s", c.name, state)
		}
		for _, dir := range []string{c.name, c.name + "-blobs"} {
			if path := holdsKey(t, filepath.Join(work, dir), citizen); path != "" {
				t.Fatalf("%s holds the citizen key", path)
			}
		}
	}
	t.Log("ok: a storage citizen names the account by its account key, and holds no copy of the citizen key")
}

// openStorage sends the ARC call that opens a session to a storage citizen,
// signed by key, straight through the relay, as a thief with a device key
// can. It returns the error code of the reply. The offer is not real, so a
// caller that has a grant gets invalid_request. The reply is sealed to key,
// so it opens only with key.
func openStorage(t *testing.T, url string, key keys.Key, storage string) string {
	t.Helper()
	provider, err := nostr.PubKeyFromHex(storage)
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(url, keyer.NewPlainKeySigner(key.Secret))
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request := call.Request{Capability: "storage", Method: "RAW", Path: "/", Body: `{"v":1,"session":{"sdp":{"type":"offer","sdp":"none"}}}`}
	reply, _, err := call.Live(ctx, key, provider, request, r)
	if err != nil {
		t.Fatalf("the storage citizen gave no reply that opens with the key %s: %v", key.Public.Hex(), err)
	}
	return reply.Err
}

// holdsKey returns a file under dir that holds key, in hex or as its 32
// bytes, or nothing.
func holdsKey(t *testing.T, dir, key string) string {
	t.Helper()
	raw, err := hex.DecodeString(key)
	if err != nil {
		t.Fatal(err)
	}
	var found string
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || found != "" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(key)) || bytes.Contains(data, raw) {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestStorageImportsDrafts proves the one-time import, and the removal of
// the drafts from the relays (docs/storage/SPEC.md, sections 4.2 and 7). An
// older relay takes the drafts of an older client. After the update the
// relay refuses new drafts, and still serves the old ones to their author.
// A new machine imports them into the local store, puts them on a storage
// citizen, and then deletes them from the relay with NIP-09. A second
// import, and a second machine of the citizen, still read every page: the
// deletion for the relay never deletes a local record.
func TestStorageImportsDrafts(t *testing.T) {
	begin := time.Now()
	work := t.TempDir()
	older := olderProgram(t, "./cmd/arc")
	relayHome := arc(t, filepath.Join(work, "relay"))
	before := relayHome.using(older).start("relay", "serve", "--listen", "127.0.0.1:0")
	if !before.waitFor("relay listens on ", 10*time.Second) {
		t.Fatalf("the older relay did not start:\n%s", before.output())
	}
	url := strings.TrimSpace(regexp.MustCompile(`relay listens on (\S+)`).FindStringSubmatch(before.output())[1])

	old := arc(t, filepath.Join(work, "old")).with(loopback...)
	old.run("keys", "gen")
	old.run("relay", "add", url)
	citizen := old.key()
	old.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	old.run("install", citizen, "journal", "--yes")

	// An older client sent each draft to the relay, and its private relay
	// list. Here one machine writes two pages, and the test sends the
	// events to the older relay as that client did.
	old.input("---\ntitle: One\npage: 1\nnotebook: hrs/old\n---\nfirst page\n", "journal", "write", "hrs/old/1")
	old.input("---\ntitle: Two\npage: 2\nnotebook: hrs/old\n---\nsecond page\n", "journal", "write", "hrs/old/2")
	stick := filepath.Join(work, "stick")
	old.run("sync", "--dir", stick)
	key, err := keyfiles.Load(keyfile(t, old.home))
	if err != nil {
		t.Fatal(err)
	}
	signer := keyer.NewPlainKeySigner(key.Secret)
	files, err := filepath.Glob(filepath.Join(stick, "events", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the stick holds no events: %v", err)
	}
	var drafts []nostr.Event
	for _, path := range files {
		var event nostr.Event
		if err := json.Unmarshal([]byte(read(t, path)), &event); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if event.PubKey == key.Public && event.Kind != nostr.KindDeletion {
			drafts = append(drafts, event)
		}
	}
	list := nostr.Event{Kind: 10013, CreatedAt: nostr.Now(), Content: "sealed", Tags: nostr.Tags{}}
	if err := signer.SignEvent(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	// The older client also used a relay that is not an ARC relay.
	standard := testrelay.StartStandard(t)
	for _, to := range []string{url, standard} {
		r := relay.New(to, signer)
		for _, event := range append(drafts, list) {
			if err := r.Send(context.Background(), event); err != nil {
				t.Fatalf("%s refused an event of kind %d: %v", to, event.Kind, err)
			}
		}
		r.Close()
	}
	onStandard := len(relaySelfData(t, standard, old.home))

	// The operator updates the relay. Its store keeps what it took.
	before.stop()
	after := relayHome.start("relay", "serve", "--listen", strings.TrimPrefix(url, "ws://"))
	if !after.waitFor("relay listens on ", 10*time.Second) {
		t.Fatalf("the relay did not start again:\n%s", after.output())
	}
	onRelay := len(relaySelfData(t, url, old.home))
	if onRelay == 0 || len(relayKinds(t, url, old.home, 10013)) != 1 {
		t.Fatalf("after the update the relay serves %d drafts of the %d that the older client sent, and %d private relay lists", onRelay, len(drafts), len(relayKinds(t, url, old.home, 10013)))
	}
	t.Logf("ok: after the update the relay still serves %d drafts and the private relay list of an older client to their author", onRelay)

	r := relay.New(url, signer)
	page, err := draft.Wrap(context.Background(), signer, "hrs/old/3", nostr.Event{Kind: 30023, CreatedAt: nostr.Now(), Content: "third page"}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Send(context.Background(), page); err == nil || !strings.Contains(err.Error(), "keeps no drafts") {
		t.Fatalf("the relay took a new draft of an older client: %v", err)
	}
	r.Close()
	t.Log("ok: the relay refuses a new draft, and says why")

	// A new machine of the citizen, with a storage citizen.
	fresh := arc(t, filepath.Join(work, "fresh")).with(loopback...)
	fresh.input(read(t, keyfile(t, old.home)), "keys", "add")
	fresh.run("relay", "add", url)
	fresh.run("relay", "add", standard)
	storeKey := storageCitizen(t, work, url, fresh)
	fresh.run("install", citizen, "journal", "--yes")
	fresh.run("install", storeKey, "storage", "--yes")

	synced := fresh.try("", "sync")
	if synced.Err != nil {
		t.Fatalf("arc sync: %v\n%s\n%s", synced.Err, synced.Stdout, synced.Stderr)
	}
	first := synced.Stdout
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

	deleted := regexp.MustCompile(`deleted (\d+) old drafts and private relay lists from the relay`).FindStringSubmatch(first)
	if deleted == nil {
		t.Fatalf("the first sync deleted no draft from the relay:\n%s", first)
	}
	if left := relaySelfData(t, url, old.home); len(left) != 0 {
		t.Fatalf("the relay holds %d drafts after the sync, want 0: kind %d", len(left), left[0].Kind)
	}
	if lists := relayKinds(t, url, old.home, 10013); len(lists) != 0 {
		t.Fatalf("the relay holds %d private relay lists after the sync, want 0", len(lists))
	}
	if roots := relayKinds(t, url, old.home, draft.Kind); len(roots) != 1 || roots[0].Tags.GetD() != "arc-keyed-root" {
		t.Fatalf("the relay holds %d drafts, want only the keyed root", len(roots))
	}
	t.Logf("ok: the sync deletes %s events from the relay: the drafts and the private relay list; the keyed root stays", deleted[1])

	// The relay that is not an ARC relay would serve the deletion to an
	// older device, so it gets none, and keeps the drafts.
	if !strings.Contains(synced.Stderr, standard+": an older or another relay; the old drafts stay on it") {
		t.Fatalf("the sync did not say that the drafts stay on %s:\n%s", standard, synced.Stderr)
	}
	if left := relaySelfData(t, standard, old.home); len(left) != onStandard {
		t.Fatalf("the relay that is not an ARC relay holds %d drafts after the sync, want the %d it had", len(left), onStandard)
	}
	// A filter with a draft kind makes the client authenticate as the
	// author.
	for _, e := range relayKinds(t, url, old.home, draft.Kind, nostr.KindDeletion) {
		if e.Kind == nostr.KindDeletion {
			t.Fatalf("the ARC relay serves a deletion to the author: %v", e.Tags)
		}
	}
	t.Log("ok: the ARC relay serves no moved deletion, and a relay that is not an ARC relay keeps its drafts and gets no deletion")

	second := fresh.run("sync", "--import")
	if strings.Contains(second, "imported") || strings.Contains(second, "deleted") {
		t.Fatalf("a second import took or deleted drafts again:\n%s", second)
	}
	if got := selfSync(t, second); got.sent != 0 || got.received != 0 {
		t.Fatalf("a second import changed the storage citizen: %+v", got)
	}
	if ls := fresh.run("journal", "ls"); strings.Count(ls, "hrs/old/") != 2 {
		t.Fatalf("the journal lists %d pages after two imports, want 2:\n%s", strings.Count(ls, "hrs/old/"), ls)
	}
	t.Log("ok: a second import takes nothing, and keeps no deletion that the relay holds")

	// A second new machine imports from the relay that holds only the
	// deletions, and reads each page from the storage citizen.
	desk := arc(t, filepath.Join(work, "desk")).with(loopback...)
	desk.input(read(t, keyfile(t, old.home)), "keys", "add")
	desk.run("relay", "add", url)
	desk.run("install", citizen, "journal", "--yes")
	desk.run("install", storeKey, "storage", "--yes")
	if got := selfSync(t, desk.run("sync")); got.received < onRelay {
		t.Fatalf("the second machine took %d records from the storage citizen, want at least %d", got.received, onRelay)
	}
	for n, text := range []string{"first page", "second page"} {
		if got := journalDoc(t, begin, desk.run("journal", "read", fmt.Sprintf("hrs/old/%d", n+1))); !strings.Contains(got, text) {
			t.Fatalf("the second machine did not read page %d:\n%s", n+1, got)
		}
	}
	t.Log("ok: a second machine of the citizen reads each page through the storage citizen")
}

// relayKinds returns the events of these kinds that the relay serves to
// the citizen of home.
func relayKinds(t *testing.T, url, home string, kinds ...nostr.Kind) []nostr.Event {
	t.Helper()
	key, err := keyfiles.Load(keyfile(t, home))
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(url, keyer.NewPlainKeySigner(key.Secret))
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	batch, err := r.Fetch(ctx, nostr.Filter{Kinds: kinds, Authors: []nostr.PubKey{key.Public}})
	if err != nil {
		t.Fatalf("the relay did not answer: %v", err)
	}
	return batch.Events
}

// TestARunningWriteKeepsItsParts runs a real arc journal write of a long
// page, and stops it after 5 of its events, before its draft. The write
// holds the lock of writes (storage spec, section 4.7). A repair 2 hours
// later keeps its parts and sends none of them, and the write then ends
// with a page that reads whole. A second write stops at the same point and
// gets SIGKILL: the next repair 2 hours later deletes its 5 parts.
func TestARunningWriteKeepsItsParts(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	laptop := arc(t, filepath.Join(work, "laptop")).with(loopback...).using(program("arc-test"))
	laptop.run("keys", "gen")
	laptop.run("relay", "add", url)
	storeKey := storageCitizen(t, work, url, laptop)
	laptop.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	laptop.run("install", laptop.key(), "journal", "--yes")
	laptop.run("install", storeKey, "storage", "--yes")
	laptop.run("sync")
	later := laptop.with("ARC_TEST_NOW_OFFSET=2h")

	page := strings.Repeat("a line of the long page\n", 40_000)
	paused := func(address string) (*exec.Cmd, string) {
		t.Helper()
		resume := filepath.Join(work, "resume-"+strings.ReplaceAll(address, "/", "-"))
		write := laptop.with("ARC_TEST_PAUSE_PUBLISH=5:"+resume).command("journal", "write", address, "--title", "Long")
		write.Stdin = strings.NewReader(page)
		errs, err := write.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := write.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = write.Process.Kill(); _ = write.Wait() })
		line, err := bufio.NewReader(errs).ReadString('\n')
		if err != nil || !strings.Contains(line, "the write waits after 5 events") {
			t.Fatalf("the write did not wait: %q, %v", line, err)
		}
		go func() { _, _ = io.Copy(io.Discard, errs) }()
		return write, resume
	}

	write, resume := paused("long/notes/1")
	out := later.run("sync", "--repair")
	if strings.Contains(out, "that no page names") {
		t.Fatalf("a repair 2 hours later deleted parts of a write that runs:\n%s", out)
	}
	write2Done := make(chan error, 1)
	if err := os.WriteFile(resume, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() { write2Done <- write.Wait() }()
	if err := <-write2Done; err != nil {
		t.Fatalf("the write that waited failed after the repair: %v", err)
	}
	if got := laptop.run("journal", "read", "long/notes/1"); !strings.HasSuffix(got, page) {
		t.Fatalf("the page of the write that waited reads %d bytes, want the whole page", len(got))
	}
	t.Log("ok: a repair 2 hours later keeps the parts of a write that runs, and the page reads whole")

	killed, _ := paused("long/notes/2")
	if err := killed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = killed.Wait()
	out = later.run("sync", "--repair")
	if !strings.Contains(out, "repair removed 5 parts and 0 checkpoints that no page names") {
		t.Fatalf("a repair 2 hours after a killed write did not delete its 5 parts:\n%s", out)
	}
	t.Log("ok: after SIGKILL, a repair 2 hours later deletes the 5 parts of the write")
}
