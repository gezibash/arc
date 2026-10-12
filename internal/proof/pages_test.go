package proof

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/draft"
)

// releasedCommit is v0.26.0, the last release whose journal pages the
// citizen key seals and signs.
const releasedCommit = "7cc7aa5a9412e8b5246470e402dbef6b2515a0b0"

// TestDevicePages proves the journal pages of the data roots through the
// normal CLI, a real relay and a storage citizen. An older arc writes two
// pages that the citizen key seals; the first sync seals them again under
// the data roots, and the second sync does nothing. With the signer
// stopped, a phone writes and reads its journal. When the signer comes
// back, the phone syncs, and the laptop reads the page of the phone, also
// after it ran an older arc that refused it. After the signer revokes the
// phone, a page that the phone writes then is refused by the other
// devices, and the page that it wrote before still opens on a new device.
func TestDevicePages(t *testing.T) {
	const limit = 10 * time.Second
	begin := time.Now()
	work := t.TempDir()
	older := olderProgramAt(t, releasedCommit, "./cmd/arc")
	url := startRelay(t, filepath.Join(work, "relay"))

	signer := arc(t, filepath.Join(work, "signer")).with(loopback...)
	signer.run("keys", "gen")
	signer.run("relay", "add", url)
	citizen := signer.key()
	storeKey := storageCitizen(t, work, url, signer)
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")
	signer.run("install", storeKey, "storage", "--yes")

	// An older arc writes the pages of the format before the data roots:
	// a short page, and a long page with parts.
	var long strings.Builder
	long.WriteString("---\ntitle: Long\npage: 2\nnotebook: hrs/old\n---\n")
	for long.Len() < 2*draft.PartSize {
		long.WriteString(strings.Repeat("z", 79) + "\n")
	}
	long.WriteString("the end of the long page\n")
	signer.using(older).input("---\ntitle: Old\npage: 1\nnotebook: hrs/old\n---\nan old page\n", "journal", "write", "hrs/old/1")
	signer.using(older).input(long.String(), "journal", "write", "hrs/old/2")
	if old, _ := pageFormats(t, signer.home, citizen); old != 2 {
		t.Fatalf("the older arc made %d pages of the old format, want 2", old)
	}

	first := signer.run("sync")
	has(t, first, "sealed 2 pages again under the data roots", "the import of the old pages")
	if old, sealed := pageFormats(t, signer.home, citizen); old != 0 || sealed != 2 {
		t.Fatalf("after the import the store holds %d pages of the old format and %d of the data roots, want 0 and 2", old, sealed)
	}
	if second := signer.run("sync"); strings.Contains(second, "sealed") {
		t.Fatalf("a second sync sealed pages again:\n%s", second)
	}
	if got := journalDoc(t, begin, signer.run("journal", "read", "hrs/old/2")); !strings.Contains(got, "the end of the long page") {
		t.Fatalf("the imported long page does not read back whole:\n%s", got[max(len(got)-200, 0):])
	}
	t.Log("ok: the first sync seals the 2 old pages again under the data roots, parts too, and the second sync does nothing")

	bunker := signer.start("keys", "bunker", "--relay", url)
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	uri = "bunker://" + uri
	phone := arc(t, filepath.Join(work, "phone")).with(loopback...)
	laptop := arc(t, filepath.Join(work, "laptop")).with(loopback...)
	phoneKey := ifaceTrim(phone.run("keys", "device"))
	laptopKey := ifaceTrim(laptop.run("keys", "device"))
	signer.run("keys", "devices", "add", phoneKey, "--name", "phone")
	signer.run("keys", "devices", "add", laptopKey, "--name", "laptop")
	if !loads(bunker, 2, limit) {
		t.Fatalf("the bunker did not load the list of two devices:\n%s", bunker.output())
	}
	for _, device := range []machine{phone, laptop} {
		device.run("keys", "add", uri)
		device.run("relay", "add", url)
		device.run("install", citizen, "journal", "--yes")
		device.run("install", storeKey, "storage", "--yes")
	}
	phone.run("sync")

	bunker.stop()
	start := time.Now()
	phone.input("---\ntitle: Offline\npage: 1\nnotebook: hrs/off\n---\nwritten with the signer stopped\n", "journal", "write", "hrs/off/1")
	if got := journalDoc(t, begin, phone.run("journal", "read", "hrs/off/1")); !strings.Contains(got, "written with the signer stopped") {
		t.Fatalf("the phone did not read its own page with the signer stopped:\n%s", got)
	}
	if got := journalDoc(t, begin, phone.run("journal", "read", "hrs/old/1")); !strings.Contains(got, "an old page") {
		t.Fatalf("the phone did not read the imported page with the signer stopped:\n%s", got)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("with the signer stopped, one write and two reads took %s", took)
	}
	t.Logf("ok: with the signer stopped, the phone writes and reads its journal in %s", time.Since(start).Round(time.Millisecond))

	bunker = signer.start("keys", "bunker", "--relay", url)
	if ifaceLine(bunker, "bunker://", limit) == "" {
		t.Fatalf("the bunker did not start again:\n%s", bunker.output())
	}
	if got := selfSync(t, phone.run("sync")); got.sent == 0 {
		t.Fatalf("the phone put nothing on the storage citizen: %+v", got)
	}
	// An older arc refuses a page that a device key signed. After its
	// update, the laptop fetches the blob again and reads the page.
	laptop.using(older).run("sync")
	if got := laptop.using(older).try("", "journal", "read", "hrs/off/1"); got.Err == nil && strings.Contains(got.Stdout, "written with the signer stopped") {
		t.Fatal("the older arc read a page that a device key signed")
	}
	laptop.run("sync")
	if got := journalDoc(t, begin, laptop.run("journal", "read", "hrs/off/1")); !strings.Contains(got, "written with the signer stopped") {
		t.Fatalf("the laptop did not read the page of the phone:\n%s", got)
	}
	t.Log("ok: when the signer comes back, the phone syncs, and the laptop reads its page, also after an older arc refused it")

	signer.run("keys", "devices", "revoke", "phone")
	if !loads(bunker, 1, limit) {
		t.Fatalf("the bunker did not load the list after the revocation:\n%s", bunker.output())
	}
	time.Sleep(time.Second) // the page below is later than the revocation
	phone.input("---\ntitle: After\npage: 1\nnotebook: hrs/after\n---\nwritten after the revocation\n", "journal", "write", "hrs/after/1")
	stick := filepath.Join(work, "stick")
	phone.run("sync", "--dir", stick)
	refused := laptop.try("", "sync", "--dir", stick)
	has(t, refused.Stderr, "the device key was revoked", "the laptop refusing the page of the revoked phone")
	if got := laptop.try("", "journal", "read", "hrs/after/1"); strings.Contains(got.Stdout, "written after the revocation") {
		t.Fatal("the laptop reads a page that the revoked phone wrote after its revocation")
	}
	t.Log("ok: the laptop refuses a page that the revoked phone wrote after its revocation")

	tablet := arc(t, filepath.Join(work, "tablet")).with(loopback...)
	tabletKey := ifaceTrim(tablet.run("keys", "device"))
	signer.run("keys", "devices", "add", tabletKey, "--name", "tablet")
	if !loads(bunker, 2, limit) {
		t.Fatalf("the bunker did not load the list with the tablet:\n%s", bunker.output())
	}
	tablet.run("keys", "add", uri)
	tablet.run("relay", "add", url)
	tablet.run("install", citizen, "journal", "--yes")
	tablet.run("install", storeKey, "storage", "--yes")
	tablet.run("sync")
	if got := journalDoc(t, begin, tablet.run("journal", "read", "hrs/off/1")); !strings.Contains(got, "written with the signer stopped") {
		t.Fatalf("the new tablet did not read the page that the phone wrote before its revocation:\n%s", got)
	}
	has(t, tablet.try("", "sync", "--dir", stick).Stderr, "the device key was revoked", "the tablet refusing the page of the revoked phone")
	t.Log("ok: a new device reads the page that the phone wrote before its revocation, and refuses the page after it")
}

// pageFormats counts the journal wraps in the store of a home: the wraps
// that the citizen key sealed to itself, and the wraps of the data roots.
func pageFormats(t *testing.T, home, citizen string) (old, sealed int) {
	t.Helper()
	pk, err := nostr.PubKeyFromHex(citizen)
	if err != nil {
		t.Fatal(err)
	}
	s, err := boltstore.Open(filepath.Join(filepath.Dir(keyfile(t, home)), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wraps, err := s.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}})
	if err != nil {
		t.Fatal(err)
	}
	for _, wrap := range wraps {
		if strings.HasPrefix(wrap.Tags.GetD(), "arc-") || draft.Owner(wrap) != pk {
			continue
		}
		if _, ok := draft.Gen(wrap); ok {
			sealed++
		} else {
			old++
		}
	}
	return old, sealed
}
