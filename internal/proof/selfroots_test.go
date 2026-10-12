package proof

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	"github.com/gezibash/arc/core/kv"
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
	account := ifaceTrim(signer.run("keys", "account", storeKey))
	blobs := filepath.Join(work, "storage-blobs", "accounts", account, "blobs")
	zero := blobsByGen(t, blobs)
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
	one := blobsByGen(t, blobs)
	if len(one[1]) == 0 {
		t.Fatalf("the phone sealed no blob with generation 1: %v", count(one))
	}
	t.Logf("ok: with the device list, the phone seals with generation 1: %v", count(one))

	has(t, signer.run("keys", "devices", "revoke", "phone"), "data roots: generation 2", "the revocation")
	if !loads(bunker, 3, limit) {
		t.Fatalf("the bunker did not load the list after the revocation:\n%s", bunker.output())
	}
	tablet.input("---\ntitle: Two\npage: 3\nnotebook: hrs/roots\n---\nafter the revocation\n",
		"journal", "write", "hrs/roots/3")
	tablet.run("sync")
	two := blobsByGen(t, blobs)
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
	found, err := filepath.Glob(filepath.Join(phone.home, "citizens", "*", "store", "data-roots.db"))
	if err != nil || len(found) != 1 {
		t.Fatalf("the phone holds %d root caches, want 1 (%v)", len(found), err)
	}
	roots := heldRoots(t, found[0])
	if _, ok := roots[2]; ok {
		t.Fatal("the revoked phone holds the data root of generation 2")
	}
	if _, ok := roots[0]; !ok {
		t.Fatalf("the phone holds no root of generation 0, so the thief test proves nothing: %v", gens(roots))
	}
	for gen, ids := range two {
		for _, id := range ids {
			blob, err := os.ReadFile(filepath.Join(blobs, id))
			if err != nil {
				t.Fatal(err)
			}
			opened := opensWithAny(roots, id, blob)
			switch {
			case gen == 2 && opened != "":
				t.Fatalf("the revoked phone opens blob %s of generation 2 with %s", id, opened)
			case gen < 2 && opened == "":
				t.Fatalf("the phone does not open blob %s of generation %d with its roots %v", id, gen, gens(roots))
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
	onlyPhoneRefused(t, "the new watch", phoneKey, watch.try("", "sync"))
	for _, page := range []string{"hrs/roots/1", "hrs/roots/3"} {
		has(t, journalDoc(t, begin, watch.run("journal", "read", page)), pages[page], "the watch reading "+page)
	}
	t.Log("ok: a device added after the revocation opens each blob of each generation, and reads each page that a listed device wrote")

	paper := arc(t, filepath.Join(work, "paper")).with(loopback...)
	recovered := ifaceTrim(paper.input(strings.Join(words, " ")+"\n", "keys", "recover"))
	if !strings.HasSuffix(recovered, citizen) {
		t.Fatalf("the seed made %q, not the citizen %s", recovered, citizen)
	}
	paper.run("relay", "add", url)
	paper.run("install", citizen, "journal", "--yes")
	paper.run("install", storeKey, "storage", "--yes")
	// The seed opens each blob. It takes no change that the revoked phone
	// signed: the device list refuses those, not the keys.
	onlyPhoneRefused(t, "the machine from the seed", phoneKey, paper.try("", "sync"))
	for _, page := range []string{"hrs/roots/1", "hrs/roots/3"} {
		has(t, journalDoc(t, begin, paper.run("journal", "read", page)), pages[page], "the seed reading "+page)
	}
	t.Log("ok: the seed alone, on a new machine, opens each blob of each generation, and reads each page that a listed device wrote")
}

// blobsByGen returns the IDs of the blob files in dir, by the generation in
// their header.
func blobsByGen(t *testing.T, dir string) map[uint32][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32][]string{}
	for _, entry := range entries {
		blob, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		gen, err := selfsync.Gen(blob)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		out[gen] = append(out[gen], entry.Name())
	}
	return out
}

func count(byGen map[uint32][]string) map[uint32]int {
	out := map[uint32]int{}
	for gen, ids := range byGen {
		out[gen] = len(ids)
	}
	return out
}

// heldRoots reads the data roots that a machine keeps on its disk.
func heldRoots(t *testing.T, path string) map[uint32][32]byte {
	t.Helper()
	db := boltkv.Open(path)
	defer db.Close()
	held := map[uint32][32]byte{}
	err := db.View(func(tx kv.Tx) error {
		b := tx.Bucket([]byte("data-roots"))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if len(k) == 4 && len(v) == 32 {
				held[binary.BigEndian.Uint32(k)] = [32]byte(v)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func gens(roots map[uint32][32]byte) []uint32 {
	var out []uint32
	for gen := range roots {
		out = append(out, gen)
	}
	return out
}

// opensWithAny tries each root as the blob key of the blob, whatever
// generation its header names. The ID key is the root of generation 0, the
// keyed root. It names the root that opens the blob, or nothing.
func opensWithAny(roots map[uint32][32]byte, id string, blob []byte) string {
	keyed := roots[0]
	gen, err := selfsync.Gen(blob)
	if err != nil {
		return ""
	}
	for g, root := range roots {
		k, err := selfsync.NewKeys(gen, root[:], keyed[:])
		if err != nil {
			continue
		}
		if _, err := selfsync.Open(k, id, blob); err == nil {
			return fmt.Sprintf("the root of generation %d", g)
		}
	}
	return ""
}

// onlyPhoneRefused checks one arc sync: it took records, and it refused no
// blob for a reason other than the device list, which refuses each change
// of the revoked phone. A blob that does not open fails the test.
func onlyPhoneRefused(t *testing.T, who, phoneKey string, synced result) {
	t.Helper()
	if got := selfSync(t, synced.Stdout); got.received == 0 {
		t.Fatalf("%s took nothing: %+v\n%s", who, got, synced.Stderr)
	}
	phone := 0
	for refusal := range strings.SplitSeq(strings.TrimSpace(synced.Stderr), "\n") {
		if !strings.Contains(refusal, "refused") {
			continue
		}
		if !strings.Contains(refusal, "not on the device list: "+phoneKey) {
			t.Fatalf("%s refused a blob for a reason other than the revoked phone:\n%s", who, synced.Stderr)
		}
		phone++
	}
	if phone == 0 {
		t.Fatalf("%s took the changes of the revoked phone:\n%s", who, synced.Stderr)
	}
}
