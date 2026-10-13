package proof

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/runtime/selfsync"
)

// TestStorageSealsWithDataRoots proves storage network step 0 through the
// normal CLI, a real relay and a storage citizen. A signer holds the
// citizen key, made from a seed. A phone and a tablet sign through its
// bunker. Self-sync blobs are sealed with the newest data root. After the
// signer revokes the phone, the tablet seals with a generation that the
// phone never held: with every root on the disk of the phone, no new blob
// opens. The tablet and the seed open every blob, also the blob of
// generation 0 from before the device list.
func TestStorageSealsWithDataRoots(t *testing.T) {
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
	storeKey := storageCitizen(t, work, url, signer)
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("install", citizen, "journal", "--yes")
	signer.run("install", storeKey, "storage", "--yes")

	// Before a device list, the newest data root is generation 0: the keyed
	// root. Its blob key is the key of each blob of an older arc.
	signer.input("---\ntitle: Zero\npage: 1\nnotebook: hrs/roots\n---\nbefore the device list\n",
		"journal", "write", "hrs/roots/1")
	if got := selfSync(t, signer.run("sync")); got.sent == 0 {
		t.Fatalf("the signer put nothing on the storage citizen: %+v", got)
	}
	zero := blobsByGen(t, signer)
	if len(zero[0]) == 0 || len(zero) != 1 {
		t.Fatalf("before the device list, the blobs by generation are %v, want generation 0 only", count(zero))
	}
	t.Log("ok: before the device list, the signer seals with generation 0")

	bunker := signer.start("keys", "bunker", "--relay", url)
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	uri = "bunker://" + uri
	phone := arc(t, filepath.Join(work, "phone")).with(loopback...)
	tablet := arc(t, filepath.Join(work, "tablet")).with(loopback...)
	phoneKey := ifaceTrim(phone.run("keys", "device"))
	tabletKey := ifaceTrim(tablet.run("keys", "device"))
	has(t, signer.run("keys", "devices", "add", phoneKey, "--name", "phone"), "data roots: generation 1", "the first add")
	has(t, signer.run("keys", "devices", "add", tabletKey, "--name", "tablet"), "data roots: generation 1", "the second add")
	if !loads(bunker, 2, limit) {
		t.Fatalf("the bunker did not load the list of two devices:\n%s", bunker.output())
	}
	for _, device := range []machine{phone, tablet} {
		device.run("keys", "add", uri)
		device.run("relay", "add", url)
		device.run("install", citizen, "journal", "--yes")
		device.run("install", storeKey, "storage", "--yes")
		device.run("sync")
	}
	phone.input("---\ntitle: One\npage: 2\nnotebook: hrs/roots\n---\nfrom the phone\n",
		"journal", "write", "hrs/roots/2")
	phone.run("sync")
	tablet.run("sync")
	one := blobsByGen(t, signer)
	if len(one[1]) == 0 {
		t.Fatalf("the phone sealed no blob with generation 1: %v", count(one))
	}
	t.Logf("ok: with the device list, the phone seals with generation 1: %v", count(one))

	// A page of the second of the revocation counts as after it.
	time.Sleep(time.Second)
	has(t, signer.run("keys", "devices", "revoke", "phone"), "data roots: generation 2", "the revocation")
	if !loads(bunker, 3, limit) {
		t.Fatalf("the bunker did not load the list after the revocation:\n%s", bunker.output())
	}
	tablet.input("---\ntitle: Two\npage: 3\nnotebook: hrs/roots\n---\nafter the revocation\n",
		"journal", "write", "hrs/roots/3")
	tablet.run("sync")
	two := blobsByGen(t, signer)
	if len(two[2]) == 0 {
		t.Fatalf("the tablet sealed no blob with generation 2 after the revocation: %v", count(two))
	}
	t.Logf("ok: after the revocation, the tablet seals with generation 2: %v", count(two))

	// The revoked phone gets no session from the bunker or the storage
	// citizen. A thief who holds its disk, and who gets the blobs from a
	// storage citizen, still has each root that the phone held. The test
	// plays that thief.
	refused := phone.try("", "sync")
	t.Logf("the revoked phone syncs: %v: %s", refused.Err, strings.TrimSpace(refused.Stderr))
	if got := phone.try("", "journal", "read", "hrs/roots/3"); strings.Contains(got.Stdout, "after the revocation") {
		t.Fatalf("the revoked phone reads the page of the tablet:\n%s", got.Stdout)
	}
	// The thief also holds the device key on the disk, which opens the
	// cache.
	roots := cachedRoots(t, phone.home)
	if _, ok := roots[2]; ok {
		t.Fatal("the revoked phone holds the data root of generation 2")
	}
	if _, ok := roots[0]; !ok {
		t.Fatalf("the phone holds no root of generation 0, so the thief test proves nothing: %v", gens(roots))
	}
	for gen, items := range two {
		for _, it := range items {
			opened := opensWithAny(roots, gen, it)
			switch {
			case gen == 2 && opened != "":
				t.Fatalf("the revoked phone opens piece %s of generation 2 with %s", it.id, opened)
			case gen < 2 && opened == "":
				t.Fatalf("the phone does not open piece %s of generation %d with its roots %v", it.id, gen, gens(roots))
			}
		}
	}
	t.Logf("ok: with each root on its disk %v, the revoked phone opens no blob of generation 2, and each blob of generations 0 and 1", gens(roots))

	pages := map[string]string{"hrs/roots/1": "before the device list", "hrs/roots/2": "from the phone", "hrs/roots/3": "after the revocation"}
	tablet.run("sync")
	for page, want := range pages {
		has(t, journalDoc(t, begin, tablet.run("journal", "read", page)), want, "the tablet reading "+page)
	}
	t.Log("ok: the tablet, which stays on the list, reads each page")

	// A device added after the revocation gets generation 2 wrapped to its
	// key, and the older roots only through the chain. It opens each blob
	// of each generation in arc sync.
	watch := arc(t, filepath.Join(work, "watch")).with(loopback...)
	watchKey := ifaceTrim(watch.run("keys", "device"))
	has(t, signer.run("keys", "devices", "add", watchKey, "--name", "watch"), "data roots: generation 2", "the add after the revocation")
	if !loads(bunker, 4, limit) {
		t.Fatalf("the bunker did not load the list with the watch:\n%s", bunker.output())
	}
	watch.run("keys", "add", uri)
	watch.run("relay", "add", url)
	watch.run("install", citizen, "journal", "--yes")
	watch.run("install", storeKey, "storage", "--yes")
	noneRefused(t, "the new watch", watch.try("", "sync"))
	for page, want := range pages {
		has(t, journalDoc(t, begin, watch.run("journal", "read", page)), want, "the watch reading "+page)
	}
	t.Log("ok: a device added after the revocation opens each blob of each generation, and reads each page, also the page that the phone wrote before its revocation")

	paper := arc(t, filepath.Join(work, "paper")).with(loopback...)
	recovered := ifaceTrim(paper.input(strings.Join(words, " ")+"\n", "keys", "recover"))
	if !strings.HasSuffix(recovered, citizen) {
		t.Fatalf("the seed made %q, not the citizen %s", recovered, citizen)
	}
	paper.run("relay", "add", url)
	paper.run("install", citizen, "journal", "--yes")
	paper.run("install", storeKey, "storage", "--yes")
	// The seed opens each blob. The page of the revoked phone is older than
	// its revocation, and comes from the storage citizen, so the device
	// list accepts it.
	noneRefused(t, "the machine from the seed", paper.try("", "sync"))
	for page, want := range pages {
		has(t, journalDoc(t, begin, paper.run("journal", "read", page)), want, "the seed reading "+page)
	}
	t.Log("ok: the seed alone, on a new machine, opens each blob of each generation, and reads each page")
}

// blobsByGen lists the pieces that the storage citizen "storage" holds
// for the signer, by the generation in their tags. A blob of format 1
// names its generation in its header.
func blobsByGen(t *testing.T, signer machine) map[uint32][]listed {
	t.Helper()
	out := map[uint32][]listed{}
	for _, it := range listAll(t, signer, "storage") {
		gen, err := genOf(it)
		if err != nil {
			t.Fatalf("%s: %v", it.id, err)
		}
		out[gen] = append(out[gen], it)
	}
	return out
}

func genOf(it listed) (uint32, error) {
	if it.tag == nil {
		return selfsync.Gen(it.data)
	}
	return selfsync.TagGen(it.tag)
}

func count(byGen map[uint32][]listed) map[uint32]int {
	out := map[uint32]int{}
	for gen, ids := range byGen {
		out[gen] = len(ids)
	}
	return out
}

func gens(roots map[uint32][32]byte) []uint32 {
	var out []uint32
	for gen := range roots {
		out = append(out, gen)
	}
	return out
}

// opensWithAny tries each root as the piece key and the blob key of a
// piece, whatever generation its tag names. The ID key is the root of
// generation 0, the keyed root. A piece of k = 1 holds the whole blob. It
// names the root that opens the piece and its blob, or nothing.
func opensWithAny(roots map[uint32][32]byte, gen uint32, it listed) string {
	keyed := roots[0]
	for g, root := range roots {
		k, err := selfsync.NewKeys(gen, root[:], keyed[:])
		if err != nil {
			continue
		}
		id, blob := it.id, it.data
		if it.tag != nil {
			h, err := selfsync.OpenTag(k, it.tag)
			if err != nil {
				continue
			}
			shard, err := selfsync.OpenPiece(k, it.tag, h, it.data)
			if err != nil {
				continue
			}
			if blob, err = selfsync.Rebuild(h, map[int][]byte{h.Index: shard}); err != nil {
				continue
			}
			id = h.Blob
		}
		if _, _, err := selfsync.Open(k, id, blob); err == nil {
			return fmt.Sprintf("the root of generation %d", g)
		}
	}
	return ""
}

// noneRefused checks one arc sync: it took records, and refused none. A
// blob that does not open, or a record that the device list refuses, fails
// the test.
func noneRefused(t *testing.T, who string, synced result) {
	t.Helper()
	if got := selfSync(t, synced.Stdout); got.received == 0 {
		t.Fatalf("%s took nothing: %+v\n%s", who, got, synced.Stderr)
	}
	if strings.Contains(synced.Stderr, "refused") {
		t.Fatalf("%s refused a blob:\n%s", who, synced.Stderr)
	}
}
