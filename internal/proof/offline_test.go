package proof

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	arckeyfile "github.com/gezibash/arc/adapters/keyfile"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
)

// TestOfflineDevice proves that a device opens the data roots that it holds
// with no path to the signer, through the normal CLI and a real relay. The
// device keeps its roots on its disk, sealed to its device key, in a file
// that only its user reads. With the signer stopped, the device opens and
// seals data of each generation that it holds. A command that needs the
// citizen key fails, and names the signer. After a revocation, the device
// keeps each older generation, and never gets the new one.
func TestOfflineDevice(t *testing.T) {
	const limit = 10 * time.Second
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	signer := arc(t, filepath.Join(work, "signer"))
	signer.run("keys", "gen")
	signer.run("relay", "add", url)
	citizen := signer.key()
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))

	bunker := signer.start("keys", "bunker", "--relay", url)
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	uri = "bunker://" + uri

	laptop := arc(t, filepath.Join(work, "laptop"))
	phone := arc(t, filepath.Join(work, "phone"))
	laptopKey := ifaceTrim(laptop.run("keys", "device"))
	phoneKey := ifaceTrim(phone.run("keys", "device"))
	has(t, signer.run("keys", "devices", "add", laptopKey, "--name", "laptop"), "data roots: generation 1", "the first add")
	has(t, signer.run("keys", "devices", "add", phoneKey, "--name", "phone"), "data roots: generation 1", "the second add")
	if !loads(bunker, 2, limit) {
		t.Fatalf("the bunker did not load the list of two devices:\n%s", bunker.output())
	}
	for _, device := range []machine{laptop, phone} {
		device.run("keys", "add", uri)
		device.run("relay", "add", url)
		device.run("install", citizen, "journal", "--yes")
		has(t, device.run("keys", "roots"), "generation 1 (newest)", "the roots of a device")
	}
	one := ifaceTrim(laptop.input("from the laptop", "keys", "seal", "journal"))
	if !strings.HasPrefix(one, "1:") {
		t.Fatalf("the laptop did not seal with generation 1: %q", one)
	}

	cache := rootCache(t, phone.home)
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the root cache %s has mode %o, want 600", cache, mode)
	}
	held := cachedRoots(t, phone.home)
	if len(held) != 2 {
		t.Fatalf("the phone keeps generations %v, want 0 and 1", gens(held))
	}
	t.Logf("ok: the phone keeps generations %v, sealed to its device key, in a file of mode 600", gens(held))

	bunker.stop()
	start := time.Now()
	has(t, phone.input(one, "keys", "unseal", "journal"), "from the laptop", "the phone opening generation 1 with the signer stopped")
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the phone waited %s to open data of a generation that it holds", took)
	}
	t.Logf("ok: with the signer stopped, the phone opens data of generation 1 in %s", time.Since(start).Round(time.Millisecond))

	sealed := phone.try("from the phone", "keys", "seal", "journal")
	if sealed.Err != nil {
		t.Fatalf("the phone did not seal with the signer stopped: %v\n%s", sealed.Err, sealed.Stderr)
	}
	if !strings.HasPrefix(sealed.Stdout, "1:") {
		t.Fatalf("the phone did not seal with generation 1: %q", sealed.Stdout)
	}
	has(t, sealed.Stderr, "the newest data roots did not come", "the warning of the phone")
	has(t, laptop.input(ifaceTrim(sealed.Stdout), "keys", "unseal", "journal"), "from the phone", "the laptop opening the data of the phone")
	t.Log("ok: with the signer stopped, the phone seals with the newest generation that it holds, and warns")

	refused := phone.try("---\ntitle: Off\npage: 1\nnotebook: hrs/offline\n---\noffline\n", "journal", "write", "hrs/offline/1")
	if refused.Err == nil {
		t.Fatal("the phone wrote a journal page with the signer stopped; the page format needs the citizen key (issue #253)")
	}
	has(t, refused.Stderr, "this needs the signer of the citizen", "the journal write with the signer stopped")
	t.Logf("ok: a journal write needs the signer, and says so: %s", strings.TrimSpace(refused.Stderr))

	bunker = signer.start("keys", "bunker", "--relay", url)
	if ifaceLine(bunker, "bunker://", limit) == "" {
		t.Fatalf("the bunker did not start again:\n%s", bunker.output())
	}
	has(t, signer.run("keys", "devices", "revoke", "phone"), "data roots: generation 2", "the revocation")
	// The new bunker process logs only the lists that it loads from now on.
	if !loads(bunker, 1, limit) {
		t.Fatalf("the bunker did not load the list after the revocation:\n%s", bunker.output())
	}
	two := ifaceTrim(laptop.input("after the revocation", "keys", "seal", "journal"))
	if !strings.HasPrefix(two, "2:") {
		t.Fatalf("the laptop did not seal with generation 2: %q", two)
	}
	has(t, phone.input(one, "keys", "unseal", "journal"), "from the laptop", "the revoked phone opening generation 1")
	denied := phone.try(two, "keys", "unseal", "journal")
	if denied.Err == nil || strings.Contains(denied.Stdout, "after the revocation") {
		t.Fatalf("the revoked phone opened generation 2: %v\n%s", denied.Err, denied.Stdout)
	}
	has(t, denied.Stderr, "no data root of generation 2", "the revoked phone opening generation 2")
	if _, ok := cachedRoots(t, phone.home)[2]; ok {
		t.Fatal("the revoked phone keeps the data root of generation 2")
	}
	t.Logf("ok: the revoked phone keeps generation 1, and cannot open generation 2: %s", strings.TrimSpace(denied.Stderr))
}

// rootCache names the root cache of the one identity of a home.
func rootCache(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(keyfile(t, home)), "store", "data-roots.db")
}

// cachedRoots opens the data roots that a device keeps on its disk, with its
// device key. Each value must be sealed: a root in clear text fails the
// test.
func cachedRoots(t *testing.T, home string) map[uint32][32]byte {
	t.Helper()
	device, err := arckeyfile.Load(filepath.Join(filepath.Dir(keyfile(t, home)), "bunker-client"))
	if err != nil {
		t.Fatal(err)
	}
	return openRoots(t, rootCache(t, home), device)
}

// openRoots reads a root cache, and opens each value with key.
func openRoots(t *testing.T, path string, key keys.Key) map[uint32][32]byte {
	t.Helper()
	db := boltkv.Open(path)
	defer db.Close()
	sealed := map[uint32]string{}
	err := db.View(func(tx kv.Tx) error {
		b := tx.Bucket([]byte("data-roots"))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			sealed[binary.BigEndian.Uint32(k)] = string(v)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	held := map[uint32][32]byte{}
	other := keys.Generate()
	for gen, value := range sealed {
		if len(value) == 32 {
			t.Fatalf("the root of generation %d is in clear text in %s", gen, path)
		}
		if _, err := other.Decrypt(context.Background(), value, key.Public); err == nil {
			t.Fatalf("another key opens the root of generation %d", gen)
		}
		text, err := key.Decrypt(context.Background(), value, key.Public)
		if err != nil {
			t.Fatalf("the root of generation %d does not open with the key of its machine: %v", gen, err)
		}
		root, err := hex.DecodeString(text)
		if err != nil || len(root) != 32 {
			t.Fatalf("the root of generation %d is not 32 bytes", gen)
		}
		held[gen] = [32]byte(root)
	}
	return held
}
