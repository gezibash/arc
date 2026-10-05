package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/apps/transfer/client"
	"github.com/gezibash/arc/apps/transfer/direct"
)

// The tests build the arc program and this program, and run them as a
// person does: a relay on this machine, three citizens, and arc serve.
var env struct {
	arc, program, state      string
	sender, receiver, other  string // arc homes
	receiverKey, strangerKey string
}

// output collects what a background process prints.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func TestMain(m *testing.M) {
	os.Exit(setup(m))
}

func setup(m *testing.M) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		return 1
	}
	root, err := os.MkdirTemp("", "arc-transfer-test")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(root)

	env.arc = filepath.Join(root, "arc")
	env.program = filepath.Join(root, "arc-transfer")
	env.state = filepath.Join(root, "state")
	for binary, pkg := range map[string]string{env.arc: "github.com/gezibash/arc/cmd/arc", env.program: "."} {
		if out, err := exec.Command("go", "build", "-o", binary, pkg).CombinedOutput(); err != nil {
			return fail("go build %s: %v\n%s", pkg, err, out)
		}
	}
	manifest, err := filepath.Abs(filepath.Join("..", "..", "manifest.json"))
	if err != nil {
		return fail("%v", err)
	}
	app := filepath.Join(root, "app")
	arcfile := fmt.Sprintf("version = 2\n\n[serve]\ncommand = %q\nmanifest = %q\n", env.program, manifest)
	if err := os.MkdirAll(app, 0o700); err != nil {
		return fail("%v", err)
	}
	if err := os.WriteFile(filepath.Join(app, "Arcfile"), []byte(arcfile), 0o600); err != nil {
		return fail("%v", err)
	}

	// A relay on a free port of this machine.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail("%v", err)
	}
	address := listener.Addr().String()
	listener.Close()
	relay := exec.Command(env.arc, "--home", filepath.Join(root, "relay"), "relay", "serve", "--listen", address)
	if err := relay.Start(); err != nil {
		return fail("arc relay serve: %v", err)
	}
	defer relay.Process.Kill()
	if !waitUntil(10*time.Second, func() bool {
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
		}
		return err == nil
	}) {
		return fail("the relay did not start")
	}

	keys := map[string]string{}
	for _, name := range []string{"sender", "receiver", "stranger"} {
		home := filepath.Join(root, name)
		for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", "ws://" + address}} {
			if out, err := exec.Command(env.arc, append([]string{"--home", home}, args...)...).CombinedOutput(); err != nil {
				return fail("arc %v: %v\n%s", args, err, out)
			}
		}
		if keys[name], err = (client.Arc{Program: env.arc, Home: home}).PublicKey(context.Background()); err != nil {
			return fail("%v", err)
		}
	}
	env.sender, env.receiver, env.other = filepath.Join(root, "sender"), filepath.Join(root, "receiver"), filepath.Join(root, "stranger")
	env.receiverKey, env.strangerKey = keys["receiver"], keys["stranger"]

	// The app of the sender. The two ends are on one machine, so they use
	// the loopback address and no STUN server.
	serve := exec.Command(env.arc, "--home", env.sender, "serve", app)
	serve.Env = append(os.Environ(), "TRANSFER_STATE="+env.state, "TRANSFER_STUN=none", "TRANSFER_LOOPBACK=1")
	served := &output{}
	serve.Stdout, serve.Stderr = served, served
	if err := serve.Start(); err != nil {
		return fail("arc serve: %v", err)
	}
	defer serve.Process.Kill()
	if !waitUntil(15*time.Second, func() bool { return strings.Contains(served.String(), "serves transfer") }) {
		return fail("the app did not start:\n%s", served.String())
	}
	for _, home := range []string{env.receiver, env.other} {
		if out, err := exec.Command(env.arc, "--home", home, "install", keys["sender"], "transfer", "--yes").CombinedOutput(); err != nil {
			return fail("arc install: %v\n%s", err, out)
		}
	}

	code := m.Run()
	if code != 0 {
		fmt.Fprintf(os.Stderr, "the log of the app:\n%s\n", served.String())
	}
	return code
}

func waitUntil(limit time.Duration, done func() bool) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if done() {
			return true
		}
	}
	return false
}

// offered writes a file of the sender, records it as an offer, and returns
// the path of the file and its link.
func offered(t *testing.T, data []byte, flags ...string) (path, link string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "photo of a bird.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"offer", "-arc", env.arc, "-home", env.sender, "-state", env.state}, flags...)
	out, err := exec.Command(env.program, append(args, path)...).Output()
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	return path, strings.TrimSpace(string(out))
}

// fetch runs arc-transfer get as a citizen, and returns what it printed on
// standard error.
func fetch(home, link, output string, flags ...string) (string, error) {
	args := append([]string{"get", "-arc", env.arc, "-home", home, "-stun", "none", "-o", output}, flags...)
	command := exec.Command(env.program, append(args, link)...)
	command.Env = append(os.Environ(), "TRANSFER_LOOPBACK=1")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	return stderr.String(), err
}

func random(t *testing.T, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func absent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s is there", filepath.Base(path))
		}
	}
}

func TestGetWritesTheBytesOfTheOfferedFile(t *testing.T) {
	// The size is not a multiple of the chunk size.
	data := random(t, 3<<20+123)
	_, link := offered(t, data)
	output := filepath.Join(t.TempDir(), "got.bin")

	if log, err := fetch(env.receiver, link, output); err != nil {
		t.Fatalf("get: %v\n%s", err, log)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the receiver has %d bytes that are not the %d bytes of the sender", len(got), len(data))
	}
	absent(t, output+".part")
}

func TestGetTakesOnlyTheRestAfterAPartFile(t *testing.T) {
	data := random(t, 3<<20)
	have := 1<<20 + 77
	_, link := offered(t, data)
	output := filepath.Join(t.TempDir(), "got.bin")
	if err := os.WriteFile(output+".part", data[:have], 0o600); err != nil {
		t.Fatal(err)
	}

	log, err := fetch(env.receiver, link, output)
	if err != nil {
		t.Fatalf("get: %v\n%s", err, log)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the file after a part file is not the file of the sender")
	}
	if want := fmt.Sprintf("%d bytes in", len(data)-have); !strings.Contains(log, want) {
		t.Fatalf("the receiver did not take only the rest, want %q in:\n%s", want, log)
	}
}

func TestGetRefusesBytesWithAWrongSHA256(t *testing.T) {
	data := random(t, 2<<20)
	_, link := offered(t, data)
	output := filepath.Join(t.TempDir(), "got.bin")
	// A part file that is not the start of the file.
	if err := os.WriteFile(output+".part", random(t, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	log, err := fetch(env.receiver, link, output)
	if err == nil {
		t.Fatalf("get took bytes with a wrong SHA-256:\n%s", log)
	}
	absent(t, output, output+".part")
}

// A sender can write more bytes than the size in its link. The receiver must
// not put these bytes on its disk.
func TestGetStopsASenderThatWritesMoreBytesThanTheLinkSays(t *testing.T) {
	data := random(t, 8<<20)
	// The limit is not at the end of a chunk.
	declared := int64(1<<20 + 100)
	_, link := offered(t, data)
	// The sender takes the size from its offer, and the receiver from the
	// link. This link says a smaller size, so the sender writes too much.
	short := strings.Replace(link, fmt.Sprintf("size=%d", len(data)), fmt.Sprintf("size=%d", declared), 1)
	if short == link {
		t.Fatalf("the link has no size to change: %s", link)
	}
	output := filepath.Join(t.TempDir(), "got.bin")

	// Watch the size of the part file while get runs.
	stop := make(chan struct{})
	watched := make(chan int64)
	go func() {
		var largest int64
		for {
			if info, err := os.Stat(output + ".part"); err == nil {
				largest = max(largest, info.Size())
			}
			select {
			case <-stop:
				watched <- largest
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	log, err := fetch(env.receiver, short, output)
	close(stop)
	largest := <-watched

	if err == nil {
		t.Fatalf("get took more bytes than the link says:\n%s", log)
	}
	if !strings.Contains(log, "more bytes than the link says") {
		t.Errorf("the receiver does not say why:\n%s", log)
	}
	if limit := declared + direct.ChunkSize; largest > limit {
		t.Errorf("the part file grew to %d bytes, the limit is %d", largest, limit)
	}
	absent(t, output, output+".part")
}

func TestOnlyANamedCitizenGetsTheFile(t *testing.T) {
	data := random(t, 1<<20)
	_, link := offered(t, data, "-to", env.receiverKey)
	dir := t.TempDir()

	stolen := filepath.Join(dir, "stolen.bin")
	if log, err := fetch(env.other, link, stolen); err == nil {
		t.Fatalf("a citizen that the offer does not name got the file:\n%s", log)
	}
	absent(t, stolen)

	output := filepath.Join(dir, "got.bin")
	if log, err := fetch(env.receiver, link, output); err != nil {
		t.Fatalf("the named citizen did not get the file: %v\n%s", err, log)
	}
	if got, _ := os.ReadFile(output); !bytes.Equal(got, data) {
		t.Fatal("the named citizen has other bytes")
	}
}

func TestTheSenderRefusesAFileThatChanged(t *testing.T) {
	path, link := offered(t, random(t, 1<<20))
	if err := os.WriteFile(path, random(t, 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "got.bin")

	log, err := fetch(env.receiver, link, output)
	if err == nil {
		t.Fatalf("get took a file that changed after the offer:\n%s", log)
	}
	if !strings.Contains(log, "changed after the offer") {
		t.Fatalf("the receiver does not say why:\n%s", log)
	}
	absent(t, output)
}

// A router can need the receiver to send the first packet. The sender then
// holds the addresses of the offer back for a time.
func TestTheSenderCanWaitBeforeItsFirstPacket(t *testing.T) {
	data := random(t, 1<<20)
	_, link := offered(t, data)
	output := filepath.Join(t.TempDir(), "got.bin")

	if log, err := fetch(env.receiver, link, output, "-hold"); err != nil {
		t.Fatalf("get -hold: %v\n%s", err, log)
	}
	if got, _ := os.ReadFile(output); !bytes.Equal(got, data) {
		t.Fatal("the receiver has other bytes")
	}
}
