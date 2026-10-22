package proof

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/core/draft"
)

// stickEvents reads the events of a directory that a citizen carries: the
// name of each file, and its text.
func stickEvents(t *testing.T, stick string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stick, "events"))
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			events[entry.Name()] = read(t, filepath.Join(stick, "events", entry.Name()))
		}
	}
	return events
}

// deletions counts the deletion requests, kind 5, among the events.
func deletions(events map[string]string) int {
	n := 0
	for _, text := range events {
		if strings.Contains(text, `"kind":5,`) {
			n++
		}
	}
	return n
}

var leftOut = regexp.MustCompile(`(\d+) events of deleted pages left out, (\d+) removed from the directory`)

// TestADirLeavesOutTheEventsOfDeletedPages proves through the normal CLI
// that arc sync --dir prints one line for the events of a deleted page,
// and removes them from the directory. The stick gets the events of a long
// page before its delete. The laptop deletes the page, and its sync removes
// the events and keeps the deletion. A second sync removes nothing. Events
// that a crash left behind go at the next sync. A phone that still holds
// the page does not put it back. A page written again at the same path
// after the delete stays, and a changed event still prints its own refusal
// and stays.
func TestADirLeavesOutTheEventsOfDeletedPages(t *testing.T) {
	begin := time.Now()
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	laptop := arc(t, filepath.Join(work, "laptop"))
	laptop.run("keys", "gen")
	laptop.run("relay", "add", url)
	citizen := laptop.key()
	laptop.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	laptop.run("install", citizen, "journal", "--yes")
	phone := arc(t, filepath.Join(work, "phone"))
	phone.input(read(t, keyfile(t, laptop.home)), "keys", "add")
	phone.run("relay", "add", url)
	phone.run("install", citizen, "journal", "--yes")
	laptop.run("relay", "rm", url)
	phone.run("relay", "rm", url)

	var long strings.Builder
	long.WriteString("---\ntitle: Gone\npage: 1\nnotebook: hrs/gone\n---\n")
	for long.Len() < 3*draft.PartSize {
		long.WriteString(strings.Repeat("g", 79) + "\n")
	}
	laptop.input(long.String(), "journal", "write", "hrs/gone/1")
	laptop.input("---\ntitle: Kept\npage: 1\nnotebook: hrs/kept\n---\nthis page stays\n", "journal", "write", "hrs/kept/1")

	stick := filepath.Join(work, "stick")
	laptop.run("sync", "--dir", stick)
	phone.run("sync", "--dir", stick)
	has(t, phone.run("journal", "read", "hrs/gone/1"), "ggg", "the page of the phone before the delete")
	before := stickEvents(t, stick)

	// The delete reaches the stick after the events of the page.
	laptop.run("journal", "delete", "hrs/gone/1")
	got := laptop.try("", "sync", "--dir", stick)
	if got.Err != nil {
		t.Fatalf("the sync after the delete failed: %v\nstdout: %s\nstderr: %s", got.Err, got.Stdout, got.Stderr)
	}
	if strings.Contains(got.Stderr, "its author deleted it") {
		t.Fatalf("the sync after the delete printed a line for each deleted event:\n%s", got.Stderr)
	}
	m := leftOut.FindStringSubmatch(got.Stdout)
	if m == nil {
		t.Fatalf("the sync after the delete printed no summary of the deleted events:\n%s", got.Stdout)
	}
	n, _ := strconv.Atoi(m[1])
	if removed, _ := strconv.Atoi(m[2]); n < 3 || removed != n {
		t.Fatalf("the sync left out %d events and removed %d; want 3 or more, all removed", n, removed)
	}
	after := stickEvents(t, stick)
	gone := 0
	for name := range before {
		if _, ok := after[name]; !ok {
			gone++
		}
	}
	if gone != n || deletions(after) <= deletions(before) {
		t.Fatalf("%d events left the stick and it holds %d deletions, before %d; want %d to leave and a new deletion", gone, deletions(after), deletions(before), n)
	}
	t.Logf("ok: one line says that %d events of the deleted page are left out, and the stick went from %d to %d events with the deletion", n, len(before), len(after))

	again := laptop.run("sync", "--dir", stick)
	if strings.Contains(again, "deleted pages") || len(stickEvents(t, stick)) != len(after) {
		t.Fatalf("a second sync removed events again:\n%s", again)
	}
	t.Log("ok: a second sync prints nothing of deleted pages, and removes nothing")

	// A crash in the removal leaves some of the files. The next sync takes them.
	var restored int
	for name, text := range before {
		if _, ok := after[name]; ok || restored == 2 {
			continue
		}
		write(t, filepath.Join(stick, "events", name), text, 0o600)
		restored++
	}
	has(t, laptop.run("sync", "--dir", stick), "2 events of deleted pages left out, 2 removed from the directory", "the sync after a removal that stopped")
	if len(stickEvents(t, stick)) != len(after) {
		t.Fatal("the sync after a removal that stopped left events of the deleted page")
	}
	t.Log("ok: the events that a stopped removal left go at the next sync")

	// A stick that holds the page and a changed copy of the deletion keeps
	// the page: the sync does not send the valid deletion under that ID.
	changedStick := filepath.Join(work, "changed-stick")
	for name, text := range before {
		write(t, filepath.Join(changedStick, "events", name), text, 0o600)
	}
	for name, text := range after {
		if _, ok := before[name]; !ok && strings.Contains(text, `"kind":5,`) {
			write(t, filepath.Join(changedStick, "events", name), strings.Replace(text, `"content":"`, `"content":"X`, 1), 0o600)
		}
	}
	kept := laptop.try("", "sync", "--dir", changedStick)
	has(t, kept.Stdout, "events of deleted pages left out; they stay in the directory, because it lacks a valid copy of a deletion request", "the sync with a changed deletion")
	for name := range before {
		if _, err := os.Stat(filepath.Join(changedStick, "events", name)); err != nil {
			t.Fatalf("the sync with a changed deletion removed %s: %v", name, err)
		}
	}
	t.Log("ok: a stick with a changed copy of the deletion keeps the events of the page")

	// The phone still holds the page. It learns of the delete from the
	// stick, and does not put the events back.
	from := phone.try("", "sync", "--dir", stick)
	if from.Err != nil || strings.Contains(from.Stderr, "its author deleted it") {
		t.Fatalf("the sync of the phone: %v\nstdout: %s\nstderr: %s", from.Err, from.Stdout, from.Stderr)
	}
	if page := phone.try("", "journal", "read", "hrs/gone/1").Stdout; page != "" {
		t.Fatalf("the phone still reads the deleted page:\n%s", page)
	}
	if len(stickEvents(t, stick)) != len(after) {
		t.Fatal("the phone put events of the deleted page back on the stick")
	}
	t.Log("ok: a phone that held the page learns the delete, and puts nothing back")

	// A page written again at the path of the deleted page stays.
	laptop.input("---\ntitle: Back\npage: 1\nnotebook: hrs/gone\n---\nwritten after the delete\n", "journal", "write", "hrs/gone/1")
	if out := laptop.run("sync", "--dir", stick); strings.Contains(out, "deleted pages") {
		t.Fatalf("the sync removed the events of the new page:\n%s", out)
	}
	phone.run("sync", "--dir", stick)
	if page := journalDoc(t, begin, phone.run("journal", "read", "hrs/gone/1")); page != "---\ntitle: Back\npage: 1\nnotebook: hrs/gone\n---\nwritten after the delete\n" {
		t.Fatalf("the phone reads the page written after the delete as:\n%s", page)
	}
	has(t, phone.run("journal", "read", "hrs/kept/1"), "this page stays", "the page that was not deleted")
	t.Log("ok: a page written again after the delete crosses the stick, and the other page stays")

	// A changed event still prints its own refusal, and stays.
	var changed string
	for name, text := range stickEvents(t, stick) {
		if strings.Contains(text, `"kind":31234`) {
			changed = name
			write(t, filepath.Join(stick, "events", name), strings.Replace(text, `"content":"`, `"content":"X`, 1), 0o600)
			break
		}
	}
	if changed == "" {
		t.Fatal("found no draft on the stick to change")
	}
	refused := laptop.try("", "sync", "--dir", stick)
	has(t, refused.Stderr, "refused "+strings.TrimSuffix(changed, ".json")+": ", "the sync with a changed event")
	if strings.Contains(refused.Stdout, "deleted pages") {
		t.Fatalf("the sync counted a changed event as deleted:\n%s", refused.Stdout)
	}
	if _, err := os.Stat(filepath.Join(stick, "events", changed)); err != nil {
		t.Fatalf("the sync removed the changed event: %v", err)
	}
	t.Log("ok: a changed event prints its own refusal, and stays on the stick")
}
