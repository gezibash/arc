package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests build the arc program and this program, and run them as a
// person does: a relay on this machine, a storage citizen that serves the
// app with arc serve, two arc homes of one owner, and a stranger.
var env struct {
	arc, program, dir  string
	owner, second      string // two arc homes with the key of the owner
	stranger           string // the arc home of another citizen
	ownerKey, storeKey string
	accountKey         string // the account key of the owner for the storage citizen
	root, relay, app   string // the temporary directory, the relay URL, the app directory
}

// quota is the quota of each account in the tests.
const quota = 64 << 10

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
	root, err := os.MkdirTemp("", "arc-storage-test")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(root)

	env.arc = filepath.Join(root, "arc")
	env.program = filepath.Join(root, "arc-storage")
	env.dir = filepath.Join(root, "blobs")
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
	env.root, env.relay, env.app = root, "ws://"+address, app
	relay := exec.Command(env.arc, "--home", filepath.Join(root, "relay"), "relay", "serve", "--listen", address, "--stun", "none")
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

	// The owner has one key in two arc homes, as two devices of one
	// citizen have before device keys exist.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fail("%v", err)
	}
	env.owner, env.second, env.stranger = filepath.Join(root, "owner"), filepath.Join(root, "second"), filepath.Join(root, "stranger")
	store := filepath.Join(root, "store")
	keys := map[string]string{}
	for _, home := range []string{env.owner, env.second, env.stranger, store} {
		add := exec.Command(env.arc, "--home", home, "keys", "gen")
		if home == env.owner || home == env.second {
			add = exec.Command(env.arc, "--home", home, "keys", "add")
			add.Stdin = strings.NewReader(hex.EncodeToString(secret))
		}
		out, err := add.Output()
		if err != nil {
			return fail("arc keys in %s: %v", home, err)
		}
		lines := strings.Fields(string(out))
		keys[home] = lines[len(lines)-1]
		if out, err := exec.Command(env.arc, "--home", home, "relay", "add", "ws://"+address).CombinedOutput(); err != nil {
			return fail("arc relay add: %v\n%s", err, out)
		}
	}
	env.ownerKey, env.storeKey = keys[env.owner], keys[store]
	if keys[env.second] != env.ownerKey {
		return fail("the two homes of the owner have two keys: %s and %s", env.ownerKey, keys[env.second])
	}

	// The owner of the account is the account key that the owner derives
	// for this storage citizen.
	account, err := exec.Command(env.arc, "--home", env.owner, "keys", "account", env.storeKey).Output()
	if err != nil {
		return fail("arc keys account: %v", err)
	}
	env.accountKey = strings.TrimSpace(string(account))

	// The storage citizen. The two ends are on one machine, so they use
	// the loopback address and no STUN server.
	serve := exec.Command(env.arc, "--home", store, "serve", app)
	serve.Env = append(os.Environ(), "STORAGE_DIR="+env.dir, "STORAGE_OWNERS="+env.accountKey,
		fmt.Sprintf("STORAGE_QUOTA=%dKiB", quota>>10), "STORAGE_STUN=none", "STORAGE_LOOPBACK=1")
	served := &output{}
	serve.Stdout, serve.Stderr = served, served
	if err := serve.Start(); err != nil {
		return fail("arc serve: %v", err)
	}
	defer serve.Process.Kill()
	if !waitUntil(15*time.Second, func() bool { return strings.Contains(served.String(), "serves storage") }) {
		return fail("the app did not start:\n%s", served.String())
	}
	for _, home := range []string{env.owner, env.second, env.stranger} {
		if out, err := exec.Command(env.arc, "--home", home, "install", env.storeKey, "storage", "--yes").CombinedOutput(); err != nil {
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

// run runs arc-storage as the citizen of an arc home, and returns its
// standard output and standard error.
func run(home string, args ...string) (stdout, stderr string, err error) {
	args = append([]string{args[0], "-arc", env.arc, "-home", home, "-stun", "none"}, args[1:]...)
	command := exec.Command(env.program, args...)
	command.Env = append(os.Environ(), "STORAGE_LOOPBACK=1")
	var out, errs bytes.Buffer
	command.Stdout, command.Stderr = &out, &errs
	err = command.Run()
	return out.String(), errs.String(), err
}

func mustRun(t *testing.T, home string, args ...string) string {
	t.Helper()
	out, errs, err := run(home, args...)
	if err != nil {
		t.Fatalf("arc-storage %s: %v\n%s", args[0], err, errs)
	}
	return out
}

// file writes random bytes to a new file, and returns its path.
func file(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("record-%d", size))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

// putIDs puts files as the owner, and returns the ID of each.
func putIDs(t *testing.T, home string, files ...string) []string {
	t.Helper()
	var ids []string
	for line := range strings.Lines(mustRun(t, home, append([]string{"put"}, files...)...)) {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[1] != "stored" {
			t.Fatalf("put printed %q, want <id> stored <file>", line)
		}
		ids = append(ids, fields[0])
	}
	if len(ids) != len(files) {
		t.Fatalf("put printed %d lines for %d files", len(ids), len(files))
	}
	return ids
}

type entry struct {
	seq, id, size string
}

func listAfter(t *testing.T, home, after string) []entry {
	t.Helper()
	var entries []entry
	for line := range strings.Lines(mustRun(t, home, "list", "-after", after)) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("list printed %q", line)
		}
		entries = append(entries, entry{fields[0], fields[1], fields[2]})
	}
	return entries
}

// TestSecondHomeSyncsFromCursor puts blobs from one home of the owner, and
// gets them from a second home of the same key, from a cursor.
func TestSecondHomeSyncsFromCursor(t *testing.T) {
	first, firstData := file(t, 100)
	second, secondData := file(t, 5000)
	ids := putIDs(t, env.owner, first, second)

	all := listAfter(t, env.second, "0")
	cursor := all[len(all)-1].seq

	third, thirdData := file(t, 3000)
	newIDs := putIDs(t, env.owner, third)

	// The second home lists from its cursor. It sees only the new blob.
	fresh := listAfter(t, env.second, cursor)
	if len(fresh) != 1 || fresh[0].id != newIDs[0] {
		t.Fatalf("list after the cursor %s gave %v, want only %s", cursor, fresh, newIDs[0])
	}
	if fresh[0].size != "4096" {
		t.Fatalf("a blob of 3000 bytes has the size %s, want 4096 (padded)", fresh[0].size)
	}

	dir := t.TempDir()
	mustRun(t, env.second, "get", "-dir", dir, ids[0], ids[1], newIDs[0])
	for id, want := range map[string][]byte{ids[0]: firstData, ids[1]: secondData, newIDs[0]: thirdData} {
		got, err := os.ReadFile(filepath.Join(dir, id))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("blob %s: the second home got %d bytes that differ from the %d bytes put", id, len(got), len(want))
		}
	}

	// On the disk of the storage citizen: one account, named by the
	// account key, not by the key of the owner, and each blob padded.
	accounts, err := os.ReadDir(filepath.Join(env.dir, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Name() != env.accountKey || env.accountKey == env.ownerKey {
		t.Fatalf("the accounts are %v, want only the account key %s for both homes", accounts, env.accountKey)
	}
	for _, id := range []string{ids[0], ids[1], newIDs[0]} {
		info, err := os.Stat(filepath.Join(env.dir, "accounts", accounts[0].Name(), "blobs", id))
		if err != nil {
			t.Fatal(err)
		}
		if size := info.Size(); size < 4096 || size&(size-1) != 0 {
			t.Fatalf("blob %s has %d bytes on disk, want a power of two of at least 4096", id, size)
		}
	}

	// A second put of a file sends no bytes.
	out := mustRun(t, env.second, "put", first)
	if !strings.Contains(out, "\theld\t") {
		t.Fatalf("a second put printed %q, want held", out)
	}
	cleanup(t, env.owner, ids[0], ids[1], newIDs[0])
}

// TestDeleteSyncs deletes a blob from one home. The other home sees the
// deletion from its cursor, and a put of the ID again is refused.
func TestDeleteSyncs(t *testing.T) {
	path, _ := file(t, 200)
	ids := putIDs(t, env.owner, path)
	before := listAfter(t, env.second, "0")
	cursor := before[len(before)-1].seq

	if out := mustRun(t, env.owner, "delete", ids[0]); !strings.Contains(out, "deleted") {
		t.Fatalf("delete printed %q", out)
	}
	fresh := listAfter(t, env.second, cursor)
	if len(fresh) != 1 || fresh[0].id != ids[0] || fresh[0].size != "deleted" {
		t.Fatalf("list after the delete gave %v, want %s deleted", fresh, ids[0])
	}
	if _, errs, err := run(env.second, "get", "-dir", t.TempDir(), ids[0]); err == nil || !strings.Contains(errs, "holds no blob") {
		t.Fatalf("get of a deleted blob: %v\n%s", err, errs)
	}
	if out := mustRun(t, env.second, "put", path); !strings.Contains(out, "\ttombstoned\t") {
		t.Fatalf("a put of a deleted ID printed %q, want tombstoned", out)
	}
}

// TestOverQuota refuses a put that passes the quota, with the bytes in use
// and the quota in the error.
func TestOverQuota(t *testing.T) {
	small, _ := file(t, 1000)
	ids := putIDs(t, env.owner, small)
	defer cleanup(t, env.owner, ids...)

	// 40000 bytes make a blob of 64 KiB, the whole quota.
	large, _ := file(t, 40000)
	_, errs, err := run(env.owner, "put", large)
	if err == nil {
		t.Fatal("a put over the quota worked")
	}
	want := fmt.Sprintf("over quota: the account uses 4096 of %d bytes, and this blob needs 65536 more", quota)
	if !strings.Contains(errs, want) {
		t.Fatalf("the error is %q, want it to contain %q", errs, want)
	}
	if out := mustRun(t, env.owner, "usage"); !strings.HasPrefix(out, fmt.Sprintf("4096 bytes of %d in use, 1 blobs", quota)) {
		t.Fatalf("usage printed %q after the refused put", out)
	}
}

// TestStrangerRefused refuses a citizen that is not an owner, before any
// pipe opens.
func TestStrangerRefused(t *testing.T) {
	path, _ := file(t, 10)
	for _, command := range []string{"put", "list"} {
		args := []string{command}
		if command == "put" {
			args = append(args, path)
		}
		_, errs, err := run(env.stranger, args...)
		if err == nil || !strings.Contains(errs, "not an owner") {
			t.Fatalf("%s as a stranger: %v\n%s", command, err, errs)
		}
		if strings.Contains(errs, "path ") {
			t.Fatalf("%s as a stranger opened a pipe:\n%s", command, errs)
		}
	}
}

// cleanup deletes blobs so that each test starts within the quota.
func cleanup(t *testing.T, home string, ids ...string) {
	t.Helper()
	mustRun(t, home, append([]string{"delete"}, ids...)...)
}
