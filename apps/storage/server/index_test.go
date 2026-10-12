package server

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/gezibash/arc/sdk/storage"
	bolt "go.etcd.io/bbolt"
)

// checkIndex checks that the index of an account agrees with its files:
// each entry has a file of its size, the bytes in use are the sum of the
// sizes, and the count is the number of entries.
func checkIndex(t *testing.T, s *store, name string) []wire.Entry {
	t.Helper()
	entries, _, err := s.list(name, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var used int64
	var count int
	for _, e := range entries {
		if e.Deleted {
			continue
		}
		info, err := os.Stat(s.blobPath(name, e.ID))
		if err != nil || info.Size() != e.Size {
			t.Fatalf("the entry of %.8s has size %d, and its file: %v", e.ID, e.Size, err)
		}
		used += e.Size
		count++
	}
	u, err := s.usage(name)
	if err != nil {
		t.Fatal(err)
	}
	if u.Used != used || u.Blobs != count {
		t.Fatalf("the index says %d bytes in %d blobs; the entries hold %d bytes in %d blobs", u.Used, u.Blobs, used, count)
	}
	return entries
}

// TestCrashMidWrite stops the program after the bytes of a put, and
// before its entry. The test runs itself as a child, which puts three
// blobs and then stops in the fourth put.
func TestCrashMidWrite(t *testing.T) {
	in := testBlobs(4, wire.MinBlob)
	if dir := os.Getenv("STORAGE_CRASH_DIR"); dir != "" {
		s := &store{dir: dir, owners: []string{testAccount}, quota: DefaultQuota, now: time.Now}
		for _, b := range in[:3] {
			if status, _, err := s.put(testAccount, b.ID, b.Data); err != nil || status != wire.StatusStored {
				fmt.Fprintln(os.Stderr, status, err)
				os.Exit(1)
			}
		}
		beforeIndex = func() { os.Exit(3) }
		_, _, err := s.put(testAccount, in[3].ID, in[3].Data)
		fmt.Fprintln(os.Stderr, "the put did not stop:", err)
		os.Exit(1)
	}

	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestCrashMidWrite$")
	child.Env = append(os.Environ(), "STORAGE_CRASH_DIR="+dir)
	out, err := child.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 3 {
		t.Fatalf("the child ended with %v, want exit 3: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts", testAccount, "blobs", in[3].ID)); err != nil {
		t.Fatalf("the file of the fourth put is not there, so the child did not stop between the bytes and the entry: %v", err)
	}

	s := &store{dir: dir, owners: []string{testAccount}, quota: DefaultQuota, now: time.Now}
	defer func() { _ = s.close() }()
	entries := checkIndex(t, s, testAccount)
	if len(entries) != 3 {
		t.Fatalf("the index holds %d entries after the stop, want 3", len(entries))
	}
	if _, err := s.get(testAccount, in[3].ID); !strings.Contains(fmt.Sprint(err), "no blob") {
		t.Fatalf("the app serves the put that stopped: %v", err)
	}
	status, seq, err := s.put(testAccount, in[3].ID, in[3].Data)
	if err != nil || status != wire.StatusStored || seq != 4 {
		t.Fatalf("the put again gave %q, seq %d, %v; want stored, seq 4", status, seq, err)
	}
	checkIndex(t, s, testAccount)
}

// oldState is a state.json of the app before index.db: an admitted
// account, a grant set, two blobs and one tombstone.
const oldState = `{"record":{"status":"admitted","quota":1048576,"plan":"friend"},
"devices":["cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"],"grants_at":100,
"seq":5,"used":12288,
"blobs":{
 "1111111111111111111111111111111111111111111111111111111111111111":{"seq":2,"size":4096,"put":"2026-10-01T10:00:00Z"},
 "2222222222222222222222222222222222222222222222222222222222222222":{"seq":5,"size":8192,"put":"2026-10-02T10:00:00Z"}},
"tombstones":{
 "3333333333333333333333333333333333333333333333333333333333333333":{"seq":4,"deleted":"2026-10-03T10:00:00Z"}}}`

// TestImportOldState starts the app on an account of an older app. The app
// imports the entries of state.json once, keeps its record and grant set,
// and serves each blob. An import that stopped before state.json was
// written does not run again.
func TestImportOldState(t *testing.T) {
	dir := t.TempDir()
	other := strings.Repeat("b", 64)
	acct := filepath.Join(dir, "accounts", other)
	if err := os.MkdirAll(filepath.Join(acct, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acct, "state.json"), []byte(oldState), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		strings.Repeat("1", 64): bytes.Repeat([]byte{1}, 4096),
		strings.Repeat("2", 64): bytes.Repeat([]byte{2}, 8192),
	}
	for id, data := range files {
		if err := os.WriteFile(filepath.Join(acct, "blobs", id), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An operator command before the first start keeps the old entries.
	plan := "family"
	if err := NewOperator(dir).Set(other, nil, 0, Change{Plan: &plan}); err != nil {
		t.Fatal(err)
	}

	s := &store{dir: dir, quota: DefaultQuota, now: time.Now}
	if n, err := s.openAll(); err != nil || n != 1 {
		t.Fatalf("openAll gave %d, %v; want 1 account", n, err)
	}
	want := []wire.Entry{
		{Seq: 2, ID: strings.Repeat("1", 64), Size: 4096},
		{Seq: 4, ID: strings.Repeat("3", 64), Deleted: true},
		{Seq: 5, ID: strings.Repeat("2", 64), Size: 8192},
	}
	entries := checkIndex(t, s, other)
	if fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Fatalf("the index lists %v, want %v", entries, want)
	}
	for id, data := range files {
		got, err := s.get(other, id)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("get %.8s gave %d bytes, %v", id, len(got), err)
		}
	}
	if status, err := s.admit(other, strings.Repeat("3", 64), 4096, 0); err != nil || status != wire.StatusTombstoned {
		t.Fatalf("a put of the tombstone gave %q, %v; want tombstoned", status, err)
	}
	u, err := s.usage(other)
	if err != nil || u.Quota != 1048576 || u.Plan != "family" || u.State != wire.StateOpen {
		t.Fatalf("usage gave %+v, %v; want the record of state.json with the plan of the operator", u, err)
	}
	if ok, err := s.granted(other, strings.Repeat("c", 64)); err != nil || !ok {
		t.Fatalf("the grant set did not survive the import: %v, %v", ok, err)
	}
	raw, err := os.ReadFile(filepath.Join(acct, "state.json"))
	if err != nil || bytes.Contains(raw, []byte("blobs")) || bytes.Contains(raw, []byte(`"seq"`)) {
		t.Fatalf("state.json still holds the old entries: %s, %v", raw, err)
	}

	// A change after the import, and then a stop before state.json was
	// written: the next start finds the old state.json again.
	if _, err := s.remove(other, []string{strings.Repeat("1", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acct, "state.json"), []byte(oldState), 0o600); err != nil {
		t.Fatal(err)
	}
	s = &store{dir: dir, quota: DefaultQuota, now: time.Now}
	defer func() { _ = s.close() }()
	if _, err := s.openAll(); err != nil {
		t.Fatal(err)
	}
	entries = checkIndex(t, s, other)
	if len(entries) != 3 || !entries[2].Deleted || entries[2].ID != strings.Repeat("1", 64) || entries[2].Seq != 6 {
		t.Fatalf("a second import changed the index: %v", entries)
	}
}

// TestIndexBytesGrowLinearly puts 10,000 blobs in one account, and counts
// the bytes that bbolt writes for the index of each put. A put writes a
// few pages, not the whole index, so the bytes of a put do not grow with
// the count of blobs. The blob files hold each blob once; the test does
// not write them.
func TestIndexBytesGrowLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("10,000 puts")
	}
	s := &store{dir: t.TempDir(), owners: []string{testAccount}, quota: DefaultQuota, now: time.Now}
	defer func() { _ = s.close() }()
	db, err := s.open(testAccount)
	if err != nil {
		t.Fatal(err)
	}
	// The test counts bytes, not syncs.
	db.NoSync = true
	written := func() int64 {
		st := db.Stats().TxStats
		// Each commit writes its dirty pages, and one meta page.
		return st.GetPageAlloc() + int64(db.Stats().TxN)*int64(db.Info().PageSize)
	}
	const puts, part = 10000, 1000
	var parts []int64
	var total int64
	start := written()
	for i := 1; i <= puts; i++ {
		id := fmt.Sprintf("%064x", i)
		err := db.Update(func(tx *bolt.Tx) error {
			_, err := index{tx}.add(id, wire.MinBlob, time.Now())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if i%part == 0 {
			now := written()
			parts = append(parts, now-start)
			total += now - start
			start = now
		}
	}
	t.Logf("index bytes for each %d puts: %v; total %d bytes, %d for each put", part, parts, total, total/puts)
	if first, last := parts[0], parts[len(parts)-1]; last > 2*first {
		t.Fatalf("the last %d puts wrote %d bytes, and the first %d wrote %d: the bytes grow faster than the count", part, last, part, first)
	}
}
