package proof

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
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
// seals data of each generation that it holds, and writes and reads its
// journal. A command that needs the citizen key, such as mail, fails, and
// names the signer. After a revocation, the device
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
	has(t, laptop.input(ifaceTrim(sealed.Stdout), "keys", "unseal", "journal"), "from the phone", "the laptop opening the data of the phone")
	t.Log("ok: with the signer stopped, the phone seals with the newest generation that it holds")

	phone.input("---\ntitle: Off\npage: 1\nnotebook: hrs/offline\n---\noffline\n", "journal", "write", "hrs/offline/1")
	has(t, phone.run("journal", "read", "hrs/offline/1"), "offline", "the journal of the phone with the signer stopped")
	refused := phone.try("", "message", "send", laptopKey, "needs", "the", "signer")
	if refused.Err == nil {
		t.Fatal("the phone sent mail with the signer stopped")
	}
	has(t, refused.Stderr, "this needs the signer of the citizen", "mail with the signer stopped")
	t.Logf("ok: a journal page needs no signer; mail needs it, and says so: %s", strings.TrimSpace(refused.Stderr))

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

// TestOfflineJournalIsQuiet proves that a journal command that works with
// no relay prints one short line about the relays, and not the error of
// each relay (#396). The home is fresh: it holds no roots event.
func TestOfflineJournalIsQuiet(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	home := arc(t, filepath.Join(work, "home"))
	home.run("keys", "gen")
	home.run("relay", "add", url)
	home.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	home.run("install", home.key(), "journal", "--yes")
	home.run("relay", "rm", url)
	for range 2 {
		home.try("", "relay", "add", "ws://"+closedPort(t))
	}
	// A relay that takes each connection and never answers times out: it
	// folds into the same line (#439).
	home.try("", "relay", "add", "ws://"+silentRelay(t))

	const offline = "arc: no relay answered, so this uses what this machine holds\n"
	for _, step := range []struct {
		stdin string
		args  []string
		out   string
	}{
		{"first page", []string{"journal", "write", "notes/day/1"}, ""},
		{"", []string{"journal", "add", "notes/day", "second line"}, ""},
		{"", []string{"journal", "read", "notes/day/1"}, "first page"},
		{"", []string{"journal", "ls"}, "notes/day/1"},
	} {
		got := home.try(step.stdin, step.args...)
		if got.Err != nil {
			t.Fatalf("arc %s offline: %v\n%s", strings.Join(step.args, " "), got.Err, got.Stderr)
		}
		if got.Stderr != offline {
			t.Errorf("arc %s offline wrote %d characters on the standard error, want the one line %q:\n%s", strings.Join(step.args, " "), len(got.Stderr), offline, got.Stderr)
		}
		if !strings.Contains(got.Stdout, step.out) {
			t.Errorf("arc %s offline printed %q, want %q in it", strings.Join(step.args, " "), got.Stdout, step.out)
		}
	}
}

// closedPort is a local address where nothing listens.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// silentRelay is a local address that takes each connection and never
// answers, as a relay behind a black hole does.
func silentRelay(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 64)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		for {
			select {
			case c := <-conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return l.Addr().String()
}

// muteRelayURL runs a relay that takes each WebSocket connection and never
// answers on it, the most common black hole, and returns its URL.
func muteRelayURL(t *testing.T) string {
	t.Helper()
	command := exec.Command(program("muterelay"))
	command.Env = append(os.Environ(), "MUTERELAY_LISTEN=127.0.0.1:0")
	out, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

// A relay that connects and never answers costs a sync one timeout, not
// one for each of its queries in series (#439): the first query that it
// does not answer marks it down. The sync says so in one line.
func TestSyncWaitsForAMuteRelayOnce(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	home := arc(t, filepath.Join(work, "home"))
	home.run("keys", "gen")
	home.run("relay", "add", url)
	home.try("", "relay", "add", muteRelayURL(t))
	start := time.Now()
	got := home.try("", "sync")
	took := time.Since(start)
	if took > 25*time.Second {
		t.Errorf("arc sync took %s with a mute relay, want about one timeout of 15 s", took)
	}
	if !strings.Contains(got.Stderr, "no answer from ws://") || strings.Count(got.Stderr, "\n") != 1 {
		t.Errorf("arc sync with a mute relay wrote %q, want one line that names it", got.Stderr)
	}
	t.Logf("ok: arc sync took %s with a mute relay", took.Round(100*time.Millisecond))
}

// Each message that waits for the relays of its recipient costs a sync
// the lookup of those relays. With a mute relay listed, that cost one
// timeout for each message, one after another (#442). After the first
// timeout the mute relay is down, so a sync with 6 waiting messages takes
// about as long as a sync with 1.
func TestPendingMessagesWaitForAMuteRelayOnce(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	home := arc(t, filepath.Join(work, "home"))
	home.run("keys", "gen")
	home.run("relay", "add", url)
	home.try("", "relay", "add", muteRelayURL(t))
	sync := func(messages int) time.Duration {
		t.Helper()
		for range messages {
			// A recipient with no relay list keeps its message waiting.
			home.try("", "message", "send", keys.Generate().Public.Hex(), "hello")
		}
		start := time.Now()
		home.try("", "sync")
		return time.Since(start)
	}
	one := sync(1)
	six := sync(5)
	if six > one+10*time.Second {
		t.Errorf("a sync with 6 waiting messages took %s, and with 1 took %s: each message waits for the mute relay", six, one)
	}
	t.Logf("ok: a sync took %s with 1 waiting message, and %s with 6", one.Round(100*time.Millisecond), six.Round(100*time.Millisecond))
}
