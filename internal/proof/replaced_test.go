package proof

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/storage"
)

// TestStorageDeletesReplaced proves section 4.7 of the storage spec
// through the normal CLI, a real relay and three storage citizens. A page
// of the journal is never replaced, so the test app of
// testdata/notes.manifest.json writes the page, as the journal of v1 did
// (k = 2, t = 3, so each blob of 4 KiB is one piece of 2,093 bytes on each
// storage citizen):
//
//   - One page is written 8 times, with a plain arc sync after each write.
//     Each write makes a new version of the draft and a checkpoint. The
//     plain syncs delete nothing, and say what waits.
//   - arc sync --repair with erin stopped deletes the 7 replaced versions
//     on charlie and dana. erin keeps them. When erin is back, the next
//     repair deletes them there. Each storage citizen then holds 8
//     checkpoints, 1 draft and the keyed root, and its usage falls by 7
//     pieces.
//   - A phone that synced after the first write, and was off for the rest,
//     reads the newest page and each of the 8 revisions. Its sync puts no
//     old version back.
//   - A new machine from the seed reads the same.
//   - The delete of the page: repair deletes each version and checkpoint.
func TestStorageDeletesReplaced(t *testing.T) {
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
	serve := map[string]func() *process{}
	running := map[string]*process{}
	names := []string{"charlie", "dana", "erin"}
	for _, name := range names {
		var key string
		key, serve[name] = storageServer(t, work, url, signer, name, program("arc-storage"), 0)
		running[name] = serve[name]()
		has(t, signer.run("keys", "storage", "add", key, "--as", name), "storage list: "+name+" is active", "the add of "+name)
	}
	// A machine from the seed installs the journal, so it is announced too.
	signer.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	signer.run("announce", filepath.Join(repo, "internal/proof/testdata/notes.manifest.json"))
	signer.run("install", citizen, "notes", "--yes")

	const page, writes = "hrs/edits/1", 8
	text := func(n int) string { return fmt.Sprintf("version %d of the page that changes", n) }
	write := func(n int) {
		t.Helper()
		signer.input(text(n)+"\n", "notes", "write", page)
	}
	sync := func(m machine, args ...string) result {
		t.Helper()
		got := m.try("", append([]string{"sync"}, args...)...)
		if got.Err != nil {
			t.Fatalf("arc sync %v: %v\n%s\n%s", args, got.Err, got.Stdout, got.Stderr)
		}
		return got
	}
	usage := func(name string) storage.Usage {
		t.Helper()
		conn := pipe(t, signer, name)
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		u, err := conn.Usage(ctx)
		if err != nil {
			t.Fatalf("the usage of %s: %v", name, err)
		}
		return u
	}
	// piece is the size of one piece of a blob of 4,096 bytes for k = 2:
	// ceil(4096 / 2) + 45 (section 3.3).
	const piece = 4096/2 + 45
	// Each storage citizen also holds the draft of the keyed root
	// (interface spec, section 3.7.1): one blob that never changes.
	const root = 1
	same := func(what string, blobs int) {
		t.Helper()
		blobs += root
		for _, name := range names {
			if u := usage(name); u.Blobs != blobs || u.Used != int64(blobs*piece) {
				t.Fatalf("%s: %s holds %d blobs of %d bytes; want %d blobs of %d bytes", what, name, u.Blobs, u.Used, blobs, blobs*piece)
			}
		}
	}

	write(1)
	sync(signer)
	phone := fromSeed(t, work, "phone", url, citizen, words)
	phone.run("install", citizen, "notes", "--yes")
	sync(phone)
	has(t, phone.run("notes", "read", page), text(1), "the first read of the phone")
	t.Log("ok: the phone holds version 1, and is off from now")

	for n := 2; n <= writes; n++ {
		write(n)
		plain := sync(signer)
		if strings.Contains(plain.Stdout, "removed") {
			t.Fatalf("a plain sync deleted blobs:\n%s", plain.Stdout)
		}
	}
	// Each write made one version of the draft and one checkpoint.
	same("before the repair", 2*writes)
	plain := sync(signer)
	has(t, plain.Stdout, fmt.Sprintf("self data: %d blobs of replaced or deleted records wait", writes-1), "a plain sync")
	same("after a plain sync", 2*writes)
	t.Logf("ok: after %d writes each storage citizen holds %d blobs of %d bytes, and a plain sync deletes none", writes, root+2*writes, (root+2*writes)*piece)

	// erin stops: the repair deletes on charlie and dana only.
	running["erin"].stop()
	cut := signer.try("", "sync", "--repair")
	t.Logf("arc sync --repair with erin stopped:\n%s\n%s", strings.TrimSpace(cut.Stdout), strings.TrimSpace(cut.Stderr))
	has(t, cut.Stdout, fmt.Sprintf("removed %d blobs of replaced records, deleted %d;", writes-1, 2*(writes-1)), "the repair with erin stopped")
	for _, name := range names[:2] {
		if u := usage(name); u.Blobs != root+writes+1 {
			t.Fatalf("%s holds %d blobs after the repair; want %d", name, u.Blobs, root+writes+1)
		}
	}
	running["erin"] = serve["erin"]()
	if u := usage("erin"); u.Blobs != root+2*writes {
		t.Fatalf("erin holds %d blobs after it was stopped; want %d", u.Blobs, root+2*writes)
	}
	t.Log("ok: the repair with erin stopped deleted the old versions on charlie and dana only")
	resumed := sync(signer, "--repair")
	t.Logf("arc sync --repair with erin back:\n%s", strings.TrimSpace(resumed.Stdout))
	has(t, resumed.Stdout, fmt.Sprintf("removed %d blobs of replaced records, deleted %d;", writes-1, writes-1), "the repair with erin back")
	same("after the repair", writes+1)
	t.Logf("ok: each storage citizen holds %d blobs of %d bytes: the keyed root, %d checkpoints and the newest draft", root+writes+1, (root+writes+1)*piece, writes)

	reads := func(m machine, what string) {
		t.Helper()
		has(t, m.run("notes", "read", page), text(writes), "the read of "+what)
		history := m.run("notes", "history", page)
		for n := 1; n <= writes; n++ {
			has(t, history, text(n), "the history of "+what)
		}
	}
	back := sync(phone)
	t.Logf("arc sync of the phone:\n%s\n%s", strings.TrimSpace(back.Stdout), strings.TrimSpace(back.Stderr))
	reads(phone, "the phone that was off")
	same("after the sync of the phone", writes+1)
	t.Log("ok: the phone that held version 1 reads version 8 and each revision, and puts no old version back")

	fresh := fromSeed(t, work, "fresh", url, citizen, words)
	fresh.run("install", citizen, "notes", "--yes")
	sync(fresh)
	reads(fresh, "a new machine")
	t.Log("ok: a new machine reads version 8 and each revision")

	// The delete of the page leaves the blank draft and the deletion.
	signer.run("notes", "delete", page)
	sync(signer)
	gone := sync(signer, "--repair")
	t.Logf("arc sync --repair after the delete:\n%s", strings.TrimSpace(gone.Stdout))
	same("after the delete", 2)
	sync(phone)
	if got := phone.try("", "notes", "read", page).Stdout; got != "" {
		t.Fatalf("the phone still reads the deleted page:\n%s", got)
	}
	same("after the phone synced the delete", 2)
	t.Log("ok: the delete of the page leaves 3 blobs on each storage citizen: the keyed root, the blank draft and the deletion")
}
