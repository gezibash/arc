package proof

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	keyfiles "github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/sdk/storage"
)

// TestStorageList proves storage network step 4 through the normal CLI, a
// real relay and three storage citizens. The signer adds two storage
// citizens with arc keys storage add, which puts them in the storage list
// of kind 10273. A phone that signs through the bunker installs both from
// the list at its sync. A machine with only the seed finds both and reads
// every page. rm sets draining, and rm --gone makes each device remove the
// install. Another citizen cannot read the list on the relay.
func TestStorageList(t *testing.T) {
	const limit = 10 * time.Second
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
	charlie := namedStorageCitizen(t, work, url, signer, "charlie")
	dana := namedStorageCitizen(t, work, url, signer, "dana")
	erin := namedStorageCitizen(t, work, url, signer, "erin")
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")

	has(t, signer.run("keys", "storage", "ls"), "the storage list is empty", "the list before an add")
	has(t, signer.run("keys", "storage", "add", charlie, "--as", "charlie"), "storage list: charlie is active", "the add of charlie")
	has(t, signer.run("keys", "storage", "add", dana, "--as", "dana"), "storage list: dana is active", "the add of dana")
	listed := signer.run("keys", "storage", "ls")
	has(t, listed, "charlie\tactive\t"+charlie, "the storage list")
	has(t, listed, "dana\tactive\t"+dana, "the storage list")
	t.Log("ok: arc keys storage add puts two storage citizens in the storage list as active")

	pages := map[string]string{"hrs/list/1": "from the signer", "hrs/list/2": "from the phone"}
	signer.input("---\ntitle: One\npage: 1\nnotebook: hrs/list\n---\nfrom the signer\n", "journal", "write", "hrs/list/1")
	synced := signer.run("sync")
	for _, name := range []string{"charlie", "dana"} {
		if got := syncOf(t, name, synced); got.sent == 0 {
			t.Fatalf("the signer put nothing on %s: %+v\n%s", name, got, synced)
		}
	}

	// A storage citizen that the signer installs by hand, and that the list
	// does not name, still syncs, and the sync says so.
	signer.run("install", erin, "storage", "--as", "erin", "--yes")
	unlisted := signer.try("", "sync")
	if unlisted.Err != nil {
		t.Fatalf("arc sync with an install that the list does not name: %v\n%s", unlisted.Err, unlisted.Stderr)
	}
	has(t, unlisted.Stderr, "erin is not in the storage list; run arc keys storage add on the signer", "the sync of the signer")
	if strings.Contains(unlisted.Stderr, "charlie is not") || strings.Contains(unlisted.Stderr, "dana is not") {
		t.Fatalf("the sync names a listed storage citizen as not in the list:\n%s", unlisted.Stderr)
	}
	syncOf(t, "erin", unlisted.Stdout)
	t.Log("ok: an install that the list does not name still syncs, and the sync says so")

	bunker := signer.start("keys", "bunker", "--relay", url)
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	phone := arc(t, filepath.Join(work, "phone")).with(loopback...)
	phoneKey := ifaceTrim(phone.run("keys", "device"))
	added := signer.run("keys", "devices", "add", phoneKey, "--name", "phone")
	has(t, added, "grants: sent to the storage citizen charlie", "the device add")
	has(t, added, "grants: sent to the storage citizen dana", "the device add")
	if !loads(bunker, 1, limit) {
		t.Fatalf("the bunker did not load the device list:\n%s", bunker.output())
	}
	phone.run("keys", "add", "bunker://"+uri)
	phone.run("relay", "add", url)
	phone.run("install", citizen, "journal", "--yes")
	first := phone.run("sync")
	has(t, first, "installed charlie from the storage list", "the first sync of the phone")
	has(t, first, "installed dana from the storage list", "the first sync of the phone")
	syncOf(t, "charlie", first)
	syncOf(t, "dana", first)
	has(t, journalDoc(t, begin, phone.run("journal", "read", "hrs/list/1")), pages["hrs/list/1"], "the phone reading hrs/list/1")
	t.Log("ok: a phone that signs through the bunker reads the storage list, installs both storage citizens, and reads the page")

	has(t, phone.refuse("keys", "storage", "rm", "charlie"), "only the signer", "a device that changes the storage list")
	t.Log("ok: a device does not change the storage list")

	phone.input("---\ntitle: Two\npage: 2\nnotebook: hrs/list\n---\nfrom the phone\n", "journal", "write", "hrs/list/2")
	phone.run("sync")

	paper := arc(t, filepath.Join(work, "paper")).with(loopback...)
	recovered := ifaceTrim(paper.input(strings.Join(words, " ")+"\n", "keys", "recover"))
	if !strings.HasSuffix(recovered, citizen) {
		t.Fatalf("the seed made %q, not the citizen %s", recovered, citizen)
	}
	paper.run("relay", "add", url)
	paper.run("install", citizen, "journal", "--yes")
	fromSeed := paper.run("sync")
	has(t, fromSeed, "installed charlie from the storage list", "the sync of the machine from the seed")
	has(t, fromSeed, "installed dana from the storage list", "the sync of the machine from the seed")
	// k is 1, so each piece comes from charlie or from dana.
	syncOf(t, "charlie", fromSeed)
	syncOf(t, "dana", fromSeed)
	if took(t, fromSeed) == 0 {
		t.Fatalf("the machine from the seed took nothing from charlie and dana:\n%s", fromSeed)
	}
	if strings.Contains(fromSeed, "erin") {
		t.Fatalf("the machine from the seed installed erin, which the list does not name:\n%s", fromSeed)
	}
	for page, want := range pages {
		has(t, journalDoc(t, begin, paper.run("journal", "read", page)), want, "the seed reading "+page)
	}
	has(t, paper.run("keys", "storage", "ls"), "dana\tactive\t"+dana, "the storage list on the machine from the seed")
	t.Log("ok: a machine with only the seed finds both storage citizens in the storage list, and reads every page")

	has(t, signer.run("keys", "storage", "rm", "dana"), "storage list: dana is draining", "the drain of dana")
	has(t, signer.run("keys", "storage", "ls"), "dana\tdraining\t"+dana, "the storage list after the drain")
	draining := phone.run("sync")
	syncOf(t, "dana", draining)
	t.Log("ok: rm sets draining, and the phone still syncs with dana")

	has(t, signer.run("keys", "storage", "rm", "dana", "--gone"), "storage list: dana is gone", "the removal of dana")
	has(t, signer.run("keys", "storage", "ls"), "dana\tgone\t"+dana, "the storage list after gone")
	gone := phone.run("sync")
	has(t, gone, "removed dana: the storage list says it is gone", "the sync of the phone after gone")
	syncOf(t, "charlie", gone)
	after := phone.run("sync")
	if strings.Contains(after, "dana") {
		t.Fatalf("the phone still syncs with dana after gone:\n%s", after)
	}
	if apps := phone.run("apps", "list"); strings.Contains(apps, "dana") || strings.Contains(apps, dana) {
		t.Fatal("the phone keeps the install of dana after gone")
	}
	t.Log("ok: rm --gone removes the install of dana on the phone at its next sync")

	// The relay serves the storage list only to its author.
	own := fetchList(t, url, signer.home, citizen)
	if own.err != nil || own.n != 1 {
		t.Fatalf("the citizen reads %d storage lists (%v); want 1", own.n, own.err)
	}
	bob := arc(t, filepath.Join(work, "bob"))
	bob.run("keys", "gen")
	stolen := fetchList(t, url, bob.home, citizen)
	if stolen.n != 0 || stolen.err == nil || !strings.Contains(stolen.err.Error(), "restricted") {
		t.Fatalf("another citizen asked for the storage list: %d events, %v; want a refusal", stolen.n, stolen.err)
	}
	t.Logf("ok: another citizen gets no storage list from the relay: %v", stolen.err)
}

type listFetch struct {
	n   int
	err error
}

// fetchList asks the relay for the storage list of citizen, as the citizen
// of home, which answers the NIP-42 challenge with its own key.
func fetchList(t *testing.T, url, home, citizen string) listFetch {
	t.Helper()
	key, err := keyfiles.Load(keyfile(t, home))
	if err != nil {
		t.Fatal(err)
	}
	author, err := nostr.PubKeyFromHex(citizen)
	if err != nil {
		t.Fatal(err)
	}
	r := relay.New(url, keyer.NewPlainKeySigner(key.Secret))
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	batch, err := r.Fetch(ctx, storage.ListFilter(author))
	if err != nil {
		return listFetch{err: err}
	}
	return listFetch{n: len(batch.Events)}
}

// syncOf reads the report line of arc sync for one storage citizen.
func syncOf(t *testing.T, name, output string) selfCounts {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(name) + `: self data received (\d+), sent (\d+), already held (\d+)`).FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("arc sync printed no report of %s:\n%s", name, output)
	}
	return selfSync(t, "storage: self data received "+m[1]+", sent "+m[2]+", already held "+m[3])
}
