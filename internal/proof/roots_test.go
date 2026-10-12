package proof

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDataRoots proves the data roots through the normal CLI and a real
// relay. A signer holds the citizen key, made from a seed. Two devices seal
// and read data of one app. Data sealed before the first device list is
// generation 0, and stays readable. After the signer revokes one device, the
// other device seals with a new generation that the revoked device cannot
// read. The seed alone, on a new machine, reads each generation.
func TestDataRoots(t *testing.T) {
	const limit = 10 * time.Second
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	signer := arc(t, filepath.Join(work, "signer"))
	made := signer.try("", "keys", "gen")
	if made.Err != nil {
		t.Fatalf("arc keys gen: %v\n%s", made.Err, made.Stderr)
	}
	words := seedWords(made.Stderr)
	signer.run("relay", "add", url)
	citizen := signer.key()

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
	for _, device := range []machine{laptop, phone} {
		device.run("keys", "add", uri)
		device.run("relay", "add", url)
	}

	zero := ifaceTrim(laptop.input("before the device list", "keys", "seal", "journal"))
	if !strings.HasPrefix(zero, "0:") {
		t.Fatalf("data sealed before the device list is not generation 0: %q", zero)
	}
	t.Log("ok: with no device list, a device seals with generation 0")

	has(t, signer.run("keys", "devices", "add", laptopKey, "--name", "laptop"), "data roots: generation 1", "the first add")
	has(t, signer.run("keys", "devices", "add", phoneKey, "--name", "phone"), "data roots: generation 1", "the second add")
	if !loads(bunker, 2, limit) {
		t.Fatalf("the bunker did not load the list of two devices:\n%s", bunker.output())
	}
	has(t, phone.run("keys", "roots"), "generation 1 (newest)", "the roots of the phone")
	held := signer.run("keys", "roots")
	has(t, held, "wrapped to laptop  "+laptopKey, "the roots on the signer")
	has(t, held, "wrapped to phone  "+phoneKey, "the roots on the signer")
	one := ifaceTrim(phone.input("from the phone", "keys", "seal", "journal"))
	if !strings.HasPrefix(one, "1:") {
		t.Fatalf("the phone did not seal with generation 1: %q", one)
	}
	has(t, laptop.input(one, "keys", "unseal", "journal"), "from the phone", "the laptop opening generation 1")
	has(t, phone.input(zero, "keys", "unseal", "journal"), "before the device list", "the phone opening generation 0")
	has(t, laptop.refuseInput(one, "keys", "unseal", "files"), "does not open", "another app opening journal data")
	t.Log("ok: two devices seal and read each other's data, of generation 0 and 1, for one app only")

	has(t, signer.run("keys", "devices", "revoke", "phone"), "data roots: generation 2", "the revocation")
	if !loads(bunker, 3, limit) {
		t.Fatalf("the bunker did not load the list after the revocation:\n%s", bunker.output())
	}
	// The phone cannot ask the signer any more, so the signer shows that
	// generation 2 has no wrap to the phone key.
	held = signer.run("keys", "roots")
	has(t, held, "generation 2 (newest)\n  wrapped to laptop  "+laptopKey+"\ngeneration 1", "the roots on the signer after the revocation")
	if strings.Contains(held, phoneKey) {
		t.Fatalf("generation 2 is wrapped to the revoked phone:\n%s", held)
	}
	two := ifaceTrim(laptop.input("after the revocation", "keys", "seal", "journal"))
	if !strings.HasPrefix(two, "2:") {
		t.Fatalf("the laptop did not seal with generation 2: %q", two)
	}
	for sealed, want := range map[string]string{zero: "before the device list", one: "from the phone", two: "after the revocation"} {
		has(t, laptop.input(sealed, "keys", "unseal", "journal"), want, "the laptop after the revocation")
	}
	refused := phone.refuseInput(two, "keys", "unseal", "journal")
	t.Logf("the phone: %s", refused)
	if strings.Contains(refused, "after the revocation") {
		t.Fatalf("the revoked phone read generation 2:\n%s", refused)
	}
	t.Log("ok: after the revocation, the laptop seals generation 2 and reads each generation; the phone cannot read generation 2")

	paper := arc(t, filepath.Join(work, "paper"))
	recovered := ifaceTrim(paper.input(strings.Join(words, " ")+"\n", "keys", "recover"))
	if !strings.HasSuffix(recovered, citizen) {
		t.Fatalf("the seed made %q, not the citizen %s", recovered, citizen)
	}
	paper.run("relay", "add", url)
	roots := paper.run("keys", "roots")
	for _, gen := range []string{"generation 2 (newest)", "generation 1", "generation 0"} {
		has(t, roots, gen, "the roots from the seed")
	}
	for sealed, want := range map[string]string{zero: "before the device list", one: "from the phone", two: "after the revocation"} {
		has(t, paper.input(sealed, "keys", "unseal", "journal"), want, "the seed")
	}
	t.Log("ok: the seed alone, on a new machine, reads each generation")
}

// refuseInput is refuse with text on standard input.
func (m machine) refuseInput(stdin string, args ...string) string {
	m.t.Helper()
	got := m.try(stdin, args...)
	if got.Err == nil {
		m.t.Fatalf("arc %s succeeded; want a failure\nstdout: %s", strings.Join(args, " "), got.Stdout)
	}
	return got.Stderr
}
