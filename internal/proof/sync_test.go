package proof

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A second sync fetches only what changed. Before, the client could not
// answer the relay's challenge on a Negentropy session, so it fetched every
// sealed event again on each sync.
func TestDeliverySealedSyncFetchesOnlyWhatChanged(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))

	a := arc(t, filepath.Join(work, "laptop"))
	b := arc(t, filepath.Join(work, "desktop"))
	a.run("keys", "gen")
	b.input(read(t, keyfile(t, a.home)), "keys", "add")
	a.run("relay", "add", url)
	b.run("relay", "add", url)

	a.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	a.run("install", a.key(), "journal", "--yes")
	b.run("install", a.key(), "journal", "--yes")

	page := func(n int) {
		a.input(fmt.Sprintf("---\ntitle: Page %d\npage: %d\nnotebook: hrs/sync\n---\nnote %d\n", n, n, n),
			"journal", "write", fmt.Sprintf("hrs/sync/%d", n))
	}
	for n := 1; n <= 5; n++ {
		page(n)
	}
	first := counts(t, b.run("sync"))
	if first.received == 0 {
		t.Fatalf("the first sync received nothing: %+v", first)
	}

	page(6)
	second := counts(t, b.run("sync"))
	if second.held != 0 {
		t.Errorf("the second sync fetched %d events that the desktop already held, want 0", second.held)
	}
	if second.received == 0 {
		t.Errorf("the second sync received nothing; the new page did not arrive")
	}
	if got := b.run("journal", "read", "hrs/sync/6"); !strings.Contains(got, "note 6") {
		t.Errorf("the desktop read %q, want the new page", got)
	}
}

type syncCounts struct{ received, sent, held int }

var syncLine = regexp.MustCompile(`sealed data received (\d+), sent (\d+), already held (\d+)`)

// counts reads the report line of arc sync, for one relay.
func counts(t *testing.T, output string) syncCounts {
	t.Helper()
	m := syncLine.FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("arc sync printed no report:\n%s", output)
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return syncCounts{n(m[1]), n(m[2]), n(m[3])}
}
