package proof

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	arckeyfile "github.com/gezibash/arc/adapters/keyfile"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/sdk/terminal"
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
	return muteRelay(t)
}

// muteRelay runs muterelay with env, and returns its URL.
func muteRelay(t *testing.T, env ...string) string {
	t.Helper()
	command := exec.Command(program("muterelay"))
	command.Env = append(append(os.Environ(), "MUTERELAY_LISTEN=127.0.0.1:0"), env...)
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

// Relays that connect and never answer cost a sync one timeout in all, not
// one for each of their queries, nor one for each relay (#439, #442): sync
// asks each relay at once first, and a relay that gives no answer, nor to
// a probe, is down. The sync names each relay that gave no answer in one
// line, with its reason.
func TestSyncWaitsForAMuteRelayOnce(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	home := arc(t, filepath.Join(work, "home"))
	home.run("keys", "gen")
	home.run("relay", "add", url)
	listRelays(t, home, muteRelayURL(t), muteRelayURL(t), "ws://"+closedPort(t))
	start := time.Now()
	got := home.try("", "sync")
	took := time.Since(start)
	// One timeout of 15 s and one probe of 5 s, for both mute relays at once.
	if took > 30*time.Second {
		t.Errorf("arc sync took %s with two mute relays, want about one timeout of 15 s and a probe of 5 s", took)
	}
	// One line for each relay that gave no answer, with its reason: the
	// closed port keeps the refusal.
	if strings.Count(got.Stderr, ": no answer; arc syncs with it again") != 2 || strings.Count(got.Stderr, ": the connection was refused; arc syncs with it again") != 1 || strings.Count(got.Stderr, "\n") != 3 {
		t.Errorf("arc sync with two mute relays and a closed port wrote %q, want one line for each, with its reason", got.Stderr)
	}
	t.Logf("ok: arc sync took %s with two mute relays and a closed port", took.Round(100*time.Millisecond))
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
	// A recipient with no relay list keeps its message waiting. The
	// messages go out before the mute relay is listed, so that each send
	// does not wait for it.
	send := func(messages int) {
		for range messages {
			home.try("", "message", "send", keys.Generate().Public.Hex(), "hello")
		}
	}
	timed := func() time.Duration {
		start := time.Now()
		home.try("", "sync")
		return time.Since(start)
	}
	send(1)
	listRelays(t, home, muteRelayURL(t))
	one := timed()
	unlistRelays(t, home, 1)
	send(5)
	listRelays(t, home, muteRelayURL(t))
	six := timed()
	if six > one+10*time.Second {
		t.Errorf("a sync with 6 waiting messages took %s, and with 1 took %s: each message waits for the mute relay", six, one)
	}
	t.Logf("ok: a sync took %s with 1 waiting message, and %s with 6", one.Round(100*time.Millisecond), six.Round(100*time.Millisecond))
}

// relaysFile is the relay list of the one identity of a home.
func relaysFile(t *testing.T, m machine) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.home, "citizens"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the home holds %d identities: %v", len(entries), err)
	}
	return filepath.Join(m.home, "citizens", entries[0].Name(), "relays")
}

// listRelays adds relays to the list of a home as arc relay add keeps
// them, without the publish of the relay list, which would wait for a relay
// that does not answer.
func listRelays(t *testing.T, m machine, urls ...string) {
	t.Helper()
	file, err := os.OpenFile(relaysFile(t, m), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, url := range urls {
		if _, err := file.WriteString(url + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// unlistRelays removes the last n relays of the list of a home.
func unlistRelays(t *testing.T, m machine, n int) {
	t.Helper()
	body, err := os.ReadFile(relaysFile(t, m))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if err := os.WriteFile(relaysFile(t, m), []byte(strings.Join(lines[:len(lines)-n], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rawErrorText is text of Go errors that a line for the user never holds.
var rawErrorText = []string{"context deadline exceeded", "failed to WebSocket dial", "error opening websocket", "dial tcp", "i/o timeout", "no relay answered: relay", "again over tcp", "can't AUTH", "the relay closed the query"}

// A sync names each relay that gave no answer, or refused, in one line of
// plain words, whatever kind of relay it is, and with or without a relay
// that answers (#442, #463). Each layout has one bad relay, so the sync
// writes one line.
func TestSyncNamesABadRelayInOneLineInEachLayout(t *testing.T) {
	work := t.TempDir()
	live := startRelay(t, filepath.Join(work, "relay"))
	// A name that does not exist gets its answer from a DNS server of the
	// test, not from the DNS of the machine, which can be slow (#442).
	notFound, mute := dnsServer(t, true), dnsServer(t, false)
	layouts := []struct {
		name, reason string
		url          func() string
		dns          string
	}{
		{"a relay that takes the WebSocket and never answers", "no answer", func() string { return muteRelayURL(t) }, ""},
		{"a host that takes the connection and never answers", "no answer", func() string { return "ws://" + silentRelay(t) }, ""},
		{"a host that does not exist", "no such host", func() string { return "ws://no-such-relay.test" }, notFound},
		{"a name that the DNS never answers", "no answer", func() string { return "ws://no-such-relay.test" }, mute},
		{"a closed port", "the connection was refused", func() string { return "ws://" + closedPort(t) }, ""},
		{"a relay that never answers a read of mail", "no answer for mail", func() string { return muteRelay(t, "MUTERELAY_MODE=mail") }, ""},
		{"a relay that closes each query as blocked", "the relay refused: blocked: this relay serves no one", func() string { return muteRelay(t, "MUTERELAY_MODE=closed") }, ""},
		{"a relay that asks for authentication and sends no challenge", "the relay refused: it asks for authentication", func() string { return muteRelay(t, "MUTERELAY_MODE=auth") }, ""},
		{"a relay that refuses the WebSocket with HTTP 403", "the relay refused: HTTP 403 for the WebSocket", func() string { return muteRelay(t, "MUTERELAY_MODE=403") }, ""},
	}
	for i, layout := range layouts {
		for _, withLive := range []bool{false, true} {
			home := arc(t, filepath.Join(work, fmt.Sprintf("home-%d-%t", i, withLive))).using(program("arc-test"))
			if layout.dns != "" {
				home = home.with("ARC_TEST_DNS=" + layout.dns)
			}
			home.run("keys", "gen")
			if withLive {
				home.run("relay", "add", live)
			}
			bad := layout.url()
			listRelays(t, home, bad)
			got := home.try("", "sync")
			want := bad + ": " + layout.reason + "; arc syncs with it again at the next sync"
			lines := strings.Split(strings.TrimRight(got.Stderr, "\n"), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], want) {
				t.Errorf("%s, with a live relay %t: arc sync wrote %q, want one line that says %q", layout.name, withLive, got.Stderr, want)
			}
			for _, raw := range rawErrorText {
				if strings.Contains(got.Stderr, raw) {
					t.Errorf("%s, with a live relay %t: arc sync wrote %q, which holds the Go text %q", layout.name, withLive, got.Stderr, raw)
				}
			}
		}
	}
}

// An arc-dm send to relays that connect and never answer waits for them
// once, all at once, and dials each of them once: the wrap goes to each
// relay at once, and a connection that never answered is not dialled
// again (#442). With a live relay, two mute relays and a closed port, it
// took two waits of 20 s, and dialled each mute relay twice.
func TestADMSendDialsEachMuteRelayOnce(t *testing.T) {
	work := t.TempDir()
	live := startRelay(t, filepath.Join(work, "relay"))
	home := arc(t, filepath.Join(work, "home"))
	home.run("keys", "gen")
	home.run("relay", "add", live)
	peer := keys.Generate().Public.Hex()
	home.run("message", "send", peer, "hello")
	accepts := []string{filepath.Join(work, "accepts-1"), filepath.Join(work, "accepts-2")}
	listRelays(t, home, muteRelay(t, "MUTERELAY_ACCEPTS="+accepts[0]), muteRelay(t, "MUTERELAY_ACCEPTS="+accepts[1]), "ws://"+closedPort(t))
	start := time.Now()
	got := home.using(program("arc-dm")).try("", "--arc", program("arc"), "--state", filepath.Join(work, "dm"), "send", peer, "again")
	took := time.Since(start)
	if got.Err != nil {
		t.Fatalf("arc-dm send: %v\n%s", got.Err, got.Stderr)
	}
	// One timeout of 15 s and one probe of 5 s, for both mute relays at once.
	if took > 30*time.Second {
		t.Errorf("arc-dm send took %s with two mute relays, want about one timeout of 15 s and a probe of 5 s", took)
	}
	for _, file := range accepts {
		body, _ := os.ReadFile(file)
		if dials := strings.Count(string(body), "accept\n"); dials != 1 {
			t.Errorf("arc-dm send dialled a mute relay %d times, want once", dials)
		}
	}
	t.Logf("ok: arc-dm send took %s with two mute relays and a closed port", took.Round(100*time.Millisecond))
}

// dnsServer is a DNS server on a local port for arc-test. When notFound,
// it answers each question at once that the name does not exist; else it
// never answers. It returns its address.
func dnsServer(t *testing.T, notFound bool) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if !notFound || n < 12 {
				continue
			}
			// The same ID and question, with the flags of an answer that
			// says the name does not exist (RCODE 3), and no records.
			answer := append([]byte(nil), buf[:n]...)
			answer[2], answer[3] = 0x81, 0x83
			clear(answer[6:12])
			_, _ = conn.WriteTo(answer, from)
		}
	}()
	return conn.LocalAddr().String()
}

// hostileReason is a reason of a refusal with terminal escapes, a bidi
// override and line breaks in it.
const hostileReason = "blocked: cato\x1b[31mRED \u202eevil\n\r\x1b[2K"

// A reason that a relay chose reaches the screen escaped once: the line of
// sync holds it raw, and is escaped where it is printed. It was escaped
// twice, so that each backslash of an escape came out doubled (#464). Each
// printed line must be exactly the line with the reason escaped once.
func TestARefusalReasonIsEscapedOnce(t *testing.T) {
	work := t.TempDir()
	live := startRelay(t, filepath.Join(work, "relay"))
	refuses := func(mode string) func() string {
		return func() string { return muteRelay(t, "MUTERELAY_MODE="+mode, "MUTERELAY_REASON="+hostileReason) }
	}
	line := func(name string) string {
		return "arc: " + terminal.EscapeLine(name+": the relay refused: "+hostileReason+"; arc syncs with it again at the next sync")
	}
	for i, layout := range []struct {
		name string
		live bool
		bad  []func() string
		mail bool
	}{
		{"a relay that refuses alone", false, []func() string{refuses("closed")}, false},
		{"a relay that refuses beside a live relay", true, []func() string{refuses("closed")}, false},
		{"two relays that refuse", false, []func() string{refuses("closed"), refuses("closed")}, false},
		{"a relay that refuses only mail", false, []func() string{refuses("mailclosed")}, true},
	} {
		home := arc(t, filepath.Join(work, fmt.Sprintf("home-%d", i)))
		home.run("keys", "gen")
		if layout.live {
			home.run("relay", "add", live)
		}
		var want []string
		for _, bad := range layout.bad {
			url := bad()
			listRelays(t, home, url)
			if layout.mail {
				url += " for mail"
			}
			want = append(want, line(url))
		}
		got := strings.Split(strings.TrimRight(home.try("", "sync").Stderr, "\n"), "\n")
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: arc sync wrote\n%s\nwant\n%s", layout.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}
