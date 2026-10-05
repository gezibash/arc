package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	arc, program, state     string
	sender, receiver, other string // arc homes
	senderKey, receiverKey  string
	strangerKey             string
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
	env.senderKey, env.receiverKey, env.strangerKey = keys["sender"], keys["receiver"], keys["stranger"]

	// The app of the sender. The two ends are on one machine, so they use
	// the loopback address and no STUN server.
	serve := exec.Command(env.arc, "--home", env.sender, "serve", app)
	serve.Env = append(os.Environ(), "TRANSFER_STATE="+env.state, "TRANSFER_STUN=none", "TRANSFER_LOOPBACK=1", "TRANSFER_PUT_MAX_MIB=4")
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

// inboxText returns the text of the first message of a sender in the inbox
// of a citizen, after a sync, or "" if there is none.
func inboxText(home, from string) string {
	_ = exec.Command(env.arc, "--home", home, "sync").Run()
	out, _ := exec.Command(env.arc, "--home", home, "message", "inbox", "--json").Output()
	for line := range strings.SplitSeq(string(out), "\n") {
		var message struct{ From, Text string }
		if json.Unmarshal([]byte(line), &message) == nil && message.From == from {
			return message.Text
		}
	}
	return ""
}

// send gives a file with one command: no service runs before it, and none
// runs after it.
func TestSendGivesAFileInOneCommand(t *testing.T) {
	data := random(t, 1<<20+5)
	path := filepath.Join(t.TempDir(), "voice note.wav")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The stranger is the sender here: no service runs for its key.
	state := filepath.Join(t.TempDir(), "state")
	send := func(wait string) (command *exec.Cmd, stdout, stderr *output) {
		command = exec.Command(env.program, "send", "-arc", env.arc, "-home", env.other, "-state", state, "-wait", wait, "-m", "listen to this", env.receiverKey, path)
		command.Env = append(os.Environ(), "TRANSFER_STUN=none", "TRANSFER_LOOPBACK=1")
		stdout, stderr = &output{}, &output{}
		command.Stdout, command.Stderr = stdout, stderr
		return command, stdout, stderr
	}

	// The receiver does not get the file in time: send says so, and fails.
	first, _, problems := send("2s")
	if err := first.Run(); err == nil || !strings.Contains(problems.String(), "did not get 1 of 1 files") {
		t.Fatalf("send with no receiver: %v\n%s", err, problems.String())
	}
	// The first send announced the app of the sender, so the receiver can
	// install it now.
	if out, err := exec.Command(env.arc, "--home", env.receiver, "install", env.strangerKey, "transfer", "--as", "transfer-of-stranger", "--yes").CombinedOutput(); err != nil {
		t.Fatalf("arc install: %v\n%s", err, out)
	}
	text := inboxText(env.receiver, env.strangerKey)
	if !strings.HasPrefix(text, "listen to this\ntransfer+arc://"+env.strangerKey+"/") {
		t.Fatalf("the message of the first send is %q", text)
	}
	link := strings.TrimPrefix(text, "listen to this\n")
	output := filepath.Join(t.TempDir(), "got.wav")
	// The service of the first send stopped with it: no process of it is
	// left, and the receiver gets no answer.
	if left, _ := exec.Command("pgrep", "-f", "arc-transfer-send").Output(); len(left) > 0 {
		t.Fatalf("a process of the service runs after send ended: %s", left)
	}
	if log, err := fetch(env.receiver, link, output); err == nil {
		t.Fatalf("a service answers after send ended:\n%s", log)
	}

	second, printed, problems := send("60s")
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- second.Wait() }()
	// The receiver gets the file while send waits. The service of send
	// answers only when it runs, so the receiver tries until then.
	var log string
	var err error
	if !waitUntil(30*time.Second, func() bool {
		log, err = fetch(env.receiver, link, output)
		return err == nil
	}) {
		t.Fatalf("get: %v\n%s", err, log)
	}
	if got, _ := os.ReadFile(output); !bytes.Equal(got, data) {
		t.Fatal("the receiver has other bytes")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send: %v\n%s", err, problems.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("send did not end after the receiver got the file:\n%s", problems.String())
	}
	if strings.TrimSpace(printed.String()) != path {
		t.Fatalf("send printed %q, want the path of the file", printed.String())
	}

	// Only the named receiver gets the file, also while a send runs.
	third, _, notes := send("20s")
	if err := third.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- third.Wait() }()
	// An interrupt lets send stop its service.
	defer func() { _ = third.Process.Signal(os.Interrupt); <-ended }()
	if !waitUntil(15*time.Second, func() bool { return strings.Contains(notes.String(), "the message is sent") }) {
		t.Fatalf("the third send did not start its service:\n%s", notes.String())
	}
	if out, err := exec.Command(env.arc, "--home", env.sender, "install", env.strangerKey, "transfer", "--as", "transfer-of-stranger", "--yes").CombinedOutput(); err != nil {
		t.Fatalf("arc install: %v\n%s", err, out)
	}
	stolen := filepath.Join(t.TempDir(), "stolen.wav")
	if log, err := fetch(env.sender, link, stolen); err == nil {
		t.Fatalf("another citizen got the file of a send:\n%s", log)
	}
	absent(t, stolen)
	// The receiver got this file before. The third send must wait for a new
	// delivery, and not end on the record of the old one.
	select {
	case err := <-ended:
		ended <- err
		t.Fatalf("a send of the same file ended with no new delivery: %v\n%s", err, notes.String())
	default:
	}
}

// give runs arc-transfer put as a citizen, to the app of the sender. It
// returns the link and what put printed on standard error.
func give(home, file string) (link, log string, err error) {
	command := exec.Command(env.program, "put", "-arc", env.arc, "-home", home, "-stun", "none", env.senderKey, file)
	command.Env = append(os.Environ(), "TRANSFER_LOOPBACK=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// A phone gives a photo to the app of an agent, and sends the link in a
// message. The agent then takes the file with get, with no connection.
func TestPutGivesAFileThatGetThenTakes(t *testing.T) {
	data := random(t, 2<<20+321)
	path := filepath.Join(t.TempDir(), "photo of a bird.jpg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	link, log, err := give(env.receiver, path)
	if err != nil {
		t.Fatalf("put: %v\n%s", err, log)
	}
	want := fmt.Sprintf("transfer+arc://%s/", env.receiverKey)
	if !strings.HasPrefix(link, want) || !strings.Contains(link, fmt.Sprintf("size=%d", len(data))) {
		t.Fatalf("put printed the link %q", link)
	}
	if _, log, err := give(env.receiver, path); err != nil || !strings.Contains(log, "has the file already") {
		t.Fatalf("a second put of the same file: %v\n%s", err, log)
	}

	output := filepath.Join(t.TempDir(), "got.jpg")
	log, err = fetch(env.sender, link, output, "-state", env.state)
	if err != nil {
		t.Fatalf("get of a file that came with put: %v\n%s", err, log)
	}
	if !strings.Contains(log, "gave the file before, with put") {
		t.Errorf("get made a connection for a file that it has:\n%s", log)
	}
	if got, _ := os.ReadFile(output); !bytes.Equal(got, data) {
		t.Fatal("the file that get took is not the file of the put")
	}
}

func TestPutGivesOnlyTheRestAfterAPartFile(t *testing.T) {
	data := random(t, 3<<20)
	have := 1<<20 + 77
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	received := direct.ReceivedFile(env.state, env.receiverKey, hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(received), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(received+".part", data[:have], 0o600); err != nil {
		t.Fatal(err)
	}

	_, log, err := give(env.receiver, path)
	if err != nil {
		t.Fatalf("put: %v\n%s", err, log)
	}
	if want := fmt.Sprintf("%d bytes in", len(data)-have); !strings.Contains(log, want) {
		t.Fatalf("put did not give only the rest, want %q in:\n%s", want, log)
	}
	if got, _ := os.ReadFile(received); !bytes.Equal(got, data) {
		t.Fatal("the app has other bytes after a part file")
	}
}

// The owner of the app sets the size of the largest file that it takes, so
// that a caller cannot fill the disk.
func TestTheAppRefusesAPutLargerThanItsLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(path, random(t, 4<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, log, err := give(env.receiver, path)
	if err == nil || !strings.Contains(log, "larger than the app takes") {
		t.Fatalf("put of a file over the limit: %v\n%s", err, log)
	}
	absent(t, filepath.Join(env.state, "received", env.receiverKey, "large.bin"))
}
