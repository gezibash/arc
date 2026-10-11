package proof

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDevices proves the device list through the normal CLI and a real
// relay. A signer holds the citizen key. Two devices with their own random
// keys sign through it. After the signer revokes one device, the signer
// refuses that device, and the other device still signs. The list survives
// a restart of the signer.
func TestDevices(t *testing.T) {
	const limit = 10 * time.Second
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	signer := arc(t, filepath.Join(work, "signer"))
	signer.run("keys", "gen")
	signer.run("relay", "add", url)
	citizen := signer.key()

	bob := arc(t, filepath.Join(work, "bob"))
	bob.run("keys", "gen")
	bob.run("relay", "add", url)
	bobKey := bob.key()

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
	if laptopKey == phoneKey || laptopKey == citizen || len(laptopKey) != 64 {
		t.Fatalf("the device keys are not separate random keys: laptop %q, phone %q, citizen %q", laptopKey, phoneKey, citizen)
	}
	if again := ifaceTrim(laptop.run("keys", "device")); again != laptopKey {
		t.Fatalf("the device key changed: %s, then %s", laptopKey, again)
	}

	signer.run("keys", "devices", "add", laptopKey, "--name", "laptop")
	signer.run("keys", "devices", "add", phoneKey, "--name", "phone")
	if !loads(bunker, 2, limit) || !strings.Contains(bunker.output(), "devices=2") {
		t.Fatalf("the bunker did not load the list of two devices:\n%s", bunker.output())
	}
	listed := signer.run("keys", "devices")
	has(t, listed, "laptop  "+laptopKey, "the device list")
	has(t, listed, "phone  "+phoneKey, "the device list")
	t.Log("ok: the signer lists two devices with their own random keys")

	for _, device := range []machine{laptop, phone} {
		added := strings.Split(ifaceTrim(device.run("keys", "add", uri)), "\n")
		if added[len(added)-1] != citizen {
			t.Fatalf("the device is %q, not the citizen %s", added, citizen)
		}
		device.run("relay", "add", url)
	}
	has(t, phone.refuse("keys", "devices", "add", laptopKey, "--name", "again"), "only the signer",
		"a device that changes the list")

	laptop.run("message", "send", bobKey, "from", "the", "laptop")
	phone.run("message", "send", bobKey, "from", "the", "phone")
	bob.run("sync")
	inbox := bob.run("message", "inbox")
	has(t, inbox, "from the laptop", "bob's inbox")
	has(t, inbox, "from the phone", "bob's inbox")
	t.Log("ok: both devices sign as the citizen through the signer")

	signer.run("keys", "devices", "revoke", "phone")
	// The first add logged devices=1 already, so wait for the third load.
	if !loads(bunker, 3, limit) {
		t.Fatalf("the bunker did not load the list after the revocation:\n%s", bunker.output())
	}
	if listed := signer.run("keys", "devices"); strings.Contains(listed, phoneKey) {
		t.Fatalf("the revoked phone is still on the list:\n%s", listed)
	}
	refused := phone.try("", "message", "send", bobKey, "after", "the", "revocation")
	if refused.Err == nil {
		t.Fatalf("the signer signed for the revoked phone:\n%s", refused.Stdout)
	}
	has(t, refused.Stderr, "not on the device list", "the refusal of the revoked phone")
	laptop.run("message", "send", bobKey, "laptop", "after", "the", "revocation")
	bob.run("sync")
	inbox = bob.run("message", "inbox")
	has(t, inbox, "laptop after the revocation", "bob's inbox")
	if strings.Count(inbox, "after the revocation") != 1 {
		t.Fatalf("a message of the revoked phone arrived:\n%s", inbox)
	}
	t.Log("ok: the signer refuses the revoked phone, and the laptop still signs")

	bunker.stop()
	bunker = signer.start("keys", "bunker", "--relay", url)
	again := ifaceLine(bunker, "bunker://", limit)
	if again == "" || strings.Contains(again, "secret=") {
		t.Fatalf("with a device list, the bunker URI must hold no secret: %q\n%s", again, bunker.output())
	}
	has(t, phone.refuse("message", "send", bobKey, "after", "the", "restart"), "not on the device list",
		"the refusal of the revoked phone after a restart")
	laptop.run("message", "send", bobKey, "laptop", "after", "the", "restart")
	t.Log("ok: the device list survives a restart of the signer")

	// A new device with the old secret URI is refused, because the list
	// admits only its devices.
	stranger := arc(t, filepath.Join(work, "stranger"))
	strangerKey := ifaceTrim(stranger.run("keys", "device"))
	refusedAdd := stranger.refuse("keys", "add", uri)
	has(t, refusedAdd, "not on the device list", "the refusal of a device that is not on the list")
	has(t, refusedAdd, "arc keys devices add "+strangerKey, "the hint for a device that is not on the list")
	t.Log("ok: the bunker secret admits no device once a list exists")
}

// has fails the test when text does not hold want.
func has(t *testing.T, text, want, what string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%s does not hold %q:\n%s", what, want, text)
	}
}

// loads waits until the bunker has loaded n device lists.
func loads(p *process, n int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if strings.Count(p.output(), "loaded the device list") >= n {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
