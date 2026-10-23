package iface

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/internal/testutil"
)

// device is a second machine of the citizen of c: the same key, and a store
// of its own.
func (c *citizen) device() *citizen {
	s, err := boltstore.Open(c.t.TempDir())
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(s.Close)
	return &citizen{t: c.t, env: &fakeEnv{t: c.t, net: c.env.net, me: c.env.me, store: s}, author: c.author}
}

// syncWith gives each store the events of the other, as arc sync does
// through a storage citizen.
func (c *citizen) syncWith(other *citizen) {
	c.t.Helper()
	for _, pair := range [][2]*citizen{{c, other}, {other, c}} {
		for _, event := range testutil.Must(pair[0].env.store.Query(nostr.Filter{})) {
			if _, err := pair[1].env.store.Save(event); err != nil {
				c.t.Fatal(err)
			}
		}
	}
}

// Two devices that append to one page while offline both keep their text
// after a sync (proposal journal-v2, item 12). Before entries, each append
// published the whole draft, and the newer draft replaced the other.
func TestTwoDevicesThatAppendOfflineKeepBothEntries(t *testing.T) {
	a := newCitizen(t)
	a.must("journal", pageHeader+"start\n", "write", page)
	b := a.device()
	a.syncWith(b)

	a.must("journal", "", "append", page, "from", "the", "laptop")
	b.env.now = a.env.Now().Add(time.Minute)
	b.must("journal", "", "append", page, "from", "the", "phone")
	a.syncWith(b)

	for name, c := range map[string]*citizen{"laptop": a, "phone": b} {
		got := c.must("journal", "", "read", page)
		if !strings.Contains(got, "from the laptop\n") || !strings.Contains(got, "from the phone\n") {
			t.Errorf("the %s reads %q, want both entries", name, got)
		}
		if !strings.HasSuffix(got, "start\nfrom the laptop\nfrom the phone\n") {
			t.Errorf("the %s reads the entries out of time order: %q", name, got)
		}
	}
}

// Two devices that start one page offline make two heads. The store keeps
// one, and history still shows the text of the other.
func TestTwoHeadsOfOnePageStayInTheHistory(t *testing.T) {
	a := newCitizen(t)
	b := a.device()
	a.must("journal", "laptop text\n", "write", page, "--title", "Laptop")
	b.env.now = a.env.Now().Add(time.Minute)
	b.must("journal", "phone text\n", "write", page, "--title", "Phone")
	a.syncWith(b)

	for _, c := range []*citizen{a, b} {
		if got := c.must("journal", "", "read", page); !strings.Contains(got, "phone text\n") {
			t.Errorf("the page reads %q, want the newer head", got)
		}
		history := c.must("journal", "", "history", page)
		if !strings.Contains(history, "laptop text") || !strings.Contains(history, "phone text") {
			t.Errorf("the history lost a head: %q", history)
		}
	}
}

// Entries of one second keep the order in which the device added them.
func TestEntriesOfOneSecondKeepTheirOrder(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", pageHeader, "write", page, "--title", "LR sweep")
	for _, word := range []string{"one", "two", "three", "four", "five"} {
		c.must("journal", "", "append", page, word)
	}
	if got := c.must("journal", "", "read", page); !strings.HasSuffix(got, "---\none\ntwo\nthree\nfour\nfive\n") {
		t.Errorf("read %q", got)
	}
}

// new writes at the number one higher than the highest of the notebook, and
// prints the address.
func TestNewWritesTheNextPage(t *testing.T) {
	c := newCitizen(t)
	if got := c.must("journal", "first\n", "new", "hrs/log", "--title", "First"); got != "hrs/log/1\n" {
		t.Fatalf("new printed %q", got)
	}
	c.must("journal", "five\n", "write", "hrs/log/5", "--title", "Five")
	c.must("journal", "other\n", "write", "hrs/other/9", "--title", "Other notebook")
	if got := c.must("journal", "sixth\n", "new", "hrs/log"); got != "hrs/log/6\n" {
		t.Fatalf("new printed %q, want hrs/log/6", got)
	}
	if got := c.must("journal", "", "read", "hrs/log/6"); !strings.Contains(got, "\npage: 6\n") || !strings.HasSuffix(got, "---\nsixth\n") {
		t.Errorf("the new page reads %q", got)
	}
	if _, err := c.run("journal", "", "new", "hrs/log"); err == nil || !strings.Contains(err.Error(), "no text") {
		t.Errorf("new with no text and no title gave %v", err)
	}
}

// add writes to the page whose title is today's date, and starts one when
// there is none.
func TestAddWritesToTodaysPage(t *testing.T) {
	c := newCitizen(t)
	c.env.now = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	c.must("journal", "older\n", "write", "hrs/log/3", "--title", "Plans")
	if got := c.must("journal", "", "add", "hrs/log", "first", "note"); got != "hrs/log/4\n" {
		t.Fatalf("add printed %q, want a new page hrs/log/4", got)
	}
	c.env.now = c.env.now.Add(time.Hour)
	if got := c.must("journal", "", "add", "hrs/log", "second", "note"); got != "hrs/log/4\n" {
		t.Fatalf("a second add printed %q, want today's page hrs/log/4", got)
	}
	got := c.must("journal", "", "read", "hrs/log/today")
	if !strings.Contains(got, "title: 2026-10-23\n") || !strings.HasSuffix(got, "---\nfirst note\nsecond note\n") {
		t.Errorf("today's page reads %q", got)
	}
	c.env.now = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	if got := c.must("journal", "", "add", "hrs/log", "next", "day"); got != "hrs/log/5\n" {
		t.Errorf("an add of the next day printed %q, want hrs/log/5", got)
	}
}

// Two devices that start today's page offline keep both texts: the text of
// an add is an entry, and only the empty heads collide.
func TestTwoDevicesThatAddOfflineKeepBothTexts(t *testing.T) {
	a := newCitizen(t)
	b := a.device()
	a.must("journal", "", "add", "hrs/log", "laptop")
	b.env.now = a.env.Now().Add(time.Minute)
	b.must("journal", "", "add", "hrs/log", "phone")
	a.syncWith(b)
	for _, c := range []*citizen{a, b} {
		if got := c.must("journal", "", "read", "hrs/log/today"); !strings.HasSuffix(got, "---\nlaptop\nphone\n") {
			t.Errorf("today's page reads %q", got)
		}
	}
}

// The words next, today and at name no page. read of a page that does not
// exist fails, so a script sees it (Cato's finding 4 of 2026-10-19: read
// printed nothing and exited 0).
func TestAMissingPageAndTheReservedWords(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "text\n", "write", page, "--title", "LR sweep")
	for _, address := range []string{"hrs/ablations/9", "hrs/ablations/today", "hrs/missing/1"} {
		if _, err := c.run("journal", "", "read", address); err == nil || err.Error() != "no such page "+address {
			t.Errorf("read %s gave %v, want no such page %s", address, err, address)
		}
	}
	if _, err := c.run("journal", "text\n", "write", "hrs/log/today", "--title", "Plans"); err == nil {
		t.Error("a write of today's page took a title that today cannot find")
	}
	for _, words := range [][]string{
		{"read", "hrs/ablations/next"},
		{"read", "hrs/ablations/at"},
		{"history", "hrs/ablations/next"},
		{"delete", "hrs/ablations/today"},
	} {
		if _, err := c.run("journal", "text\n", words...); err == nil || !strings.Contains(err.Error(), "names no page") {
			t.Errorf("%v gave %v, want: names no page", words, err)
		}
	}
	// The address patterns of the manifest refuse the other words.
	for _, words := range [][]string{
		{"write", "hrs/ablations/at"},
		{"append", "hrs/ablations/next", "text"},
		{"fix", "hrs/ablations/today", "text"},
	} {
		if _, err := c.run("journal", "text\n", words...); err == nil {
			t.Errorf("%v names a page", words)
		}
	}
	if _, err := c.run("journal", "", "append", "hrs/ablations/9", "text"); err == nil || err.Error() != "no such page hrs/ablations/9" {
		t.Errorf("an append to a missing page gave %v", err)
	}
}

// historyOf reads the history of a page as records.
func historyOf(c *citizen, address string) []map[string]any {
	c.t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(c.must("journal", "", "history", address, "--json")), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

// A fix takes the place of the entry that it corrects, and the history
// keeps both. A fix of a fix corrects the first entry.
func TestAFixReplacesAnEntryAndTheHistoryKeepsBoth(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", pageHeader+"start\n", "write", page)
	c.must("journal", "", "append", page, "teh", "typo")
	c.must("journal", "", "append", page, "after")
	history := historyOf(c, page)
	if len(history) != 3 {
		t.Fatalf("the history has %d records, want the head and 2 entries", len(history))
	}
	typo := history[1]["tags"].(map[string]any)["d"].(string)

	c.must("journal", "", "fix", typo, "the", "fix")
	if got := c.must("journal", "", "read", page); !strings.HasSuffix(got, "---\nstart\nthe fix\nafter\n") {
		t.Errorf("after the fix, read %q", got)
	}
	history = historyOf(c, page)
	if len(history) != 4 || !strings.Contains(c.must("journal", "", "history", page), "teh typo") {
		t.Errorf("the history lost the entry that the fix corrects: %v", history)
	}
	second := history[3]["tags"].(map[string]any)["d"].(string)
	c.must("journal", "", "fix", second, "the", "second", "fix")
	if got := c.must("journal", "", "read", page); !strings.HasSuffix(got, "---\nstart\nthe second fix\nafter\n") {
		t.Errorf("after a fix of the fix, read %q", got)
	}
	var page1 map[string]any
	json.Unmarshal([]byte(c.must("journal", "", "read", page, "--json")), &page1)
	entries := page1["entries"].([]any)
	if last := entries[len(entries)-1].(map[string]any); last["corrects"] != typo || last["device"] != c.env.me.Public().Hex() {
		t.Errorf("the last entry is %v, want a fix of %s by this device", last, typo)
	}

	// A fix of the head replaces the text after its front matter.
	c.must("journal", "", "fix", page, "new", "start")
	if got := c.must("journal", "", "read", page); !strings.Contains(got, "---\nnew start\nthe second fix\nafter\n") {
		t.Errorf("after a fix of the head, read %q", got)
	}
	if _, err := c.run("journal", "", "fix", page+"/0123456789abcdef", "text"); err == nil {
		t.Error("a fix of an entry that does not exist was kept")
	}
}

// A delete takes the head, each entry, and the parts of a long entry.
func TestADeleteTakesEachEntry(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", pageHeader+"start\n", "write", page)
	c.must("journal", "", "append", page, strings.Repeat("long ", draft.PartSize/2))
	c.must("journal", "", "append", page, "short")
	if n := countKind(c, draft.PartKind); n == 0 {
		t.Fatal("the long entry has no parts")
	}
	c.must("journal", "", "delete", page)
	if n := countKind(c, draft.PartKind); n != 0 {
		t.Errorf("%d parts stay after the delete", n)
	}
	entries := testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"k": {"4272"}}}))
	if len(entries) != 0 {
		t.Errorf("%d entries stay after the delete", len(entries))
	}
	if _, err := c.run("journal", "", "read", page); err == nil {
		t.Error("the deleted page reads")
	}
	// The page can start again, with no entry of the old one.
	c.must("journal", pageHeader+"again\n", "write", page)
	if got := c.must("journal", "", "read", page); got != pageHeader+"again\n" {
		t.Errorf("the new page reads %q", got)
	}
}

// ls orders by the time of the newest entry with --order updated.
func TestLsOrdersByTheNewestEntry(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "one\n", "write", "hrs/log/1", "--title", "One")
	c.env.now = c.env.Now().Add(time.Hour)
	c.must("journal", "two\n", "write", "hrs/log/2", "--title", "Two")
	c.env.now = c.env.Now().Add(time.Hour)
	c.must("journal", "", "append", "hrs/log/1", "later")
	got := c.must("journal", "", "ls", "--notebook", "hrs/log", "--order", "updated")
	if !strings.HasPrefix(got, "hrs/log/2\t") || !strings.Contains(got, "hrs/log/1\tOne\t2026-09-29T12:00:00Z\t2026-09-29T14:00:00Z") {
		t.Errorf("ls --order updated shows %q", got)
	}
}

// The time of an entry is in seconds, and the time of the head is not. An
// entry of the same second as the head never shows an updated_at earlier
// than created_at.
func TestUpdatedAtIsNeverEarlierThanTheHead(t *testing.T) {
	c := newCitizen(t)
	c.env.now = time.Date(2026, 9, 29, 13, 0, 0, 500_000_000, time.UTC)
	c.must("journal", "text\n", "write", page, "--title", "Same second")
	c.must("journal", "", "append", page, "an", "entry")
	got := c.must("journal", "", "read", page)
	if !strings.Contains(got, "created_at: 2026-09-29T13:00:00.5Z\nupdated_at: 2026-09-29T13:00:00.5Z\n") {
		t.Errorf("read %q, want updated_at no earlier than created_at", got)
	}
	c.env.now = c.env.now.Add(2 * time.Second)
	c.must("journal", "", "append", page, "later")
	if got := c.must("journal", "", "read", page); !strings.Contains(got, "updated_at: 2026-09-29T13:00:02Z\n") {
		t.Errorf("read %q, want the time of the newest entry", got)
	}
}

// A sync can bring the entries of a page before its head. A write of that
// address then fails, so the entries do not join a new head.
func TestAWriteOfAPageWithOnlyEntriesFails(t *testing.T) {
	a := newCitizen(t)
	a.must("journal", "text\n", "write", page, "--title", "Head")
	a.must("journal", "", "append", page, "an", "entry")
	b := a.device()
	for _, event := range testutil.Must(a.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"k": {"4272"}}})) {
		if _, err := b.env.store.Save(event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.run("journal", "other\n", "write", page, "--title", "Other"); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("a write of a page with only entries gave %v, want: the page exists", err)
	}
}

// ls shows no updated_at earlier than created_at for an entry of the same
// second as the head.
func TestLsKeepsTheHeadTimeForAnEntryOfTheSameSecond(t *testing.T) {
	c := newCitizen(t)
	c.env.now = time.Date(2026, 9, 29, 13, 0, 0, 500_000_000, time.UTC)
	c.must("journal", "text\n", "write", page, "--title", "Same second")
	c.must("journal", "", "append", page, "an", "entry")
	if got := c.must("journal", "", "ls"); !strings.Contains(got, "\t2026-09-29T13:00:00.5Z\t2026-09-29T13:00:00.5Z") {
		t.Errorf("ls shows %q, want updated_at no earlier than created_at", got)
	}
}
