package iface

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	if got := c.must("journal", "", "read", page); !strings.Contains(got, "updated_at: 2026-09-29T13:00:02.5Z\n") {
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

// runAll runs a command, and returns its output, its standard error and its
// error.
func (c *citizen) runAll(stdin string, words ...string) (string, string, error) {
	c.t.Helper()
	var out, errs bytes.Buffer
	err := Run(context.Background(), c.env, Installed{Manifest: specManifests(c.t)["journal"], Author: c.author, Name: "journal"}, words,
		Stdio{In: strings.NewReader(stdin), Out: &out, Err: &errs})
	return out.String(), errs.String(), err
}

// Two devices that start the next page of one notebook offline take the
// same number. After a sync, each page keeps its own text and entries: the
// store keeps one head at the number, and the other page reads at an
// address of its own, which read and ls name (Cato's test of #395 at
// d4476bd: D1's entries read under D2's head, and D1's head had no address).
func TestTwoOfflineNewsKeepTheirOwnPages(t *testing.T) {
	a := newCitizen(t)
	b := a.device()
	if got := a.must("journal", "laptop text\n", "new", "hrs/log", "--title", "Laptop"); got != "hrs/log/1\n" {
		t.Fatalf("new printed %q", got)
	}
	a.must("journal", "", "append", "hrs/log/1", "laptop", "entry")
	b.env.now = a.env.Now().Add(time.Minute)
	if got := b.must("journal", "phone text\n", "new", "hrs/log", "--title", "Phone"); got != "hrs/log/1\n" {
		t.Fatalf("new printed %q", got)
	}
	b.must("journal", "", "append", "hrs/log/1", "phone", "entry")
	a.syncWith(b)

	for name, c := range map[string]*citizen{"laptop": a, "phone": b} {
		// The store keeps the newer head: the phone's.
		got, errs, err := c.runAll("", "read", "hrs/log/1")
		if err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		if !strings.HasSuffix(got, "---\nphone text\nphone entry\n") || strings.Contains(got, "laptop") {
			t.Errorf("%s: hrs/log/1 reads %q, want the phone's page alone", name, got)
		}
		if !strings.Contains(errs, "hrs/log/1-") {
			t.Errorf("%s: read does not name the address of the other page: %q", name, errs)
		}
		ls := c.must("journal", "", "ls", "--notebook", "hrs/log")
		var other string
		for line := range strings.SplitSeq(ls, "\n") {
			if address, _, _ := strings.Cut(line, "\t"); strings.HasPrefix(address, "hrs/log/1-") {
				other = address
			}
		}
		if other == "" || !strings.Contains(ls, "\tLaptop\t") {
			t.Fatalf("%s: ls does not list the laptop's page: %q", name, ls)
		}
		if got := c.must("journal", "", "read", other); !strings.HasSuffix(got, "---\nlaptop text\nlaptop entry\n") || strings.Contains(got, "phone") {
			t.Errorf("%s: %s reads %q, want the laptop's page alone", name, other, got)
		}
	}
}

// A device that adds a burst of entries, and another device that adds one
// entry less than a second later, read in time order after a sync (Cato's
// test of #395 at d4476bd: each entry of a burst was one second after the
// one before, so a later entry of the other device read in the middle).
func TestABurstKeepsTimeOrderWithTheOtherDevice(t *testing.T) {
	a := newCitizen(t)
	a.must("journal", "start\n", "write", page, "--title", "Burst")
	b := a.device()
	a.syncWith(b)
	burst := a.env.Now()
	b.env.now = burst
	for i := 1; i <= 6; i++ {
		b.must("journal", "", "append", page, fmt.Sprintf("B%d", i))
	}
	a.env.now = burst.Add(330 * time.Millisecond)
	a.must("journal", "", "append", page, "A1")
	a.syncWith(b)
	for name, c := range map[string]*citizen{"laptop": a, "phone": b} {
		got := c.must("journal", "", "read", page)
		if !strings.HasSuffix(got, "---\nstart\nB1\nB2\nB3\nB4\nB5\nB6\nA1\n") {
			t.Errorf("%s reads %q, want the burst, then A1", name, got)
		}
		if !strings.Contains(got, "updated_at: "+burst.Add(330*time.Millisecond).UTC().Format(time.RFC3339Nano)+"\n") {
			t.Errorf("%s: updated_at is not the time of A1: %q", name, got)
		}
	}
}

// One device deletes a page while the other appends to it, in either
// order. After a sync the page is gone, the entry that the delete did not
// know reads at an address of its own, and ls lists it (Cato's test of #395
// at d4476bd: the page was left with no head and no listing).
func TestADeleteAndAnOfflineAppendEndClear(t *testing.T) {
	for _, appendFirst := range []bool{true, false} {
		a := newCitizen(t)
		a.must("journal", "start\n", "write", page, "--title", "Doomed")
		b := a.device()
		a.syncWith(b)
		b.env.now = a.env.Now().Add(time.Minute)
		if appendFirst {
			b.must("journal", "", "append", page, "kept", "entry")
			a.env.now = a.env.Now().Add(2 * time.Minute)
			a.must("journal", "", "delete", page)
		} else {
			a.must("journal", "", "delete", page)
			b.must("journal", "", "append", page, "kept", "entry")
		}
		a.syncWith(b)
		for name, c := range map[string]*citizen{"deleter": a, "appender": b} {
			if _, err := c.run("journal", "", "read", page); err == nil || err.Error() != "no such page "+page {
				t.Errorf("append first %v, %s: read gave %v, want no such page", appendFirst, name, err)
			}
			ls := c.must("journal", "", "ls", "--notebook", "hrs/ablations")
			address, _, _ := strings.Cut(ls, "\t")
			if !strings.HasPrefix(address, page+"-") {
				t.Fatalf("append first %v, %s: ls shows %q, want the entry at an address of its own", appendFirst, name, ls)
			}
			if got := c.must("journal", "", "read", address); !strings.Contains(got, "kept entry\n") {
				t.Errorf("append first %v, %s: %s reads %q", appendFirst, name, address, got)
			}
			// The address of the page is free again.
			c.must("journal", "fresh\n", "write", page, "--title", "Fresh")
			c.must("journal", "", "delete", page)
		}
	}
}

// A page number too large to have a next one is refused (Cato's test of
// #395 at d4476bd: new and /next then failed with an overflow).
func TestAPageNumberTooLargeIsRefused(t *testing.T) {
	c := newCitizen(t)
	if _, err := c.run("journal", "text\n", "write", "hrs/log/9223372036854775807", "--title", "Huge"); err == nil {
		t.Fatal("a write of page 9223372036854775807 was kept")
	}
	if got := c.must("journal", "text\n", "write", "hrs/log/999999999", "--title", "Last"); got != "" {
		t.Fatalf("write printed %q", got)
	}
	if _, err := c.run("journal", "text\n", "new", "hrs/log"); err == nil {
		t.Error("new wrote past the highest page number")
	}
}

// lostAddress is the address of the lost page of an address, from ls.
func lostAddress(c *citizen, address string) string {
	c.t.Helper()
	for line := range strings.SplitSeq(c.must("journal", "", "ls"), "\n") {
		if a, _, _ := strings.Cut(line, "\t"); strings.HasPrefix(a, address+"-") {
			return a
		}
	}
	c.t.Fatalf("ls lists no lost page of %s", address)
	return ""
}

// A lost page takes entries and fixes of its own. A delete of the live page
// leaves the lost page, and a delete of the lost page leaves the live one.
func TestALostPageLivesOnItsOwn(t *testing.T) {
	a := newCitizen(t)
	b := a.device()
	a.must("journal", "laptop text\n", "new", "hrs/log", "--title", "Laptop")
	b.env.now = a.env.Now().Add(time.Minute)
	b.must("journal", "phone text\n", "new", "hrs/log", "--title", "Phone")
	a.syncWith(b)

	lost := lostAddress(a, "hrs/log/1")
	if got := a.must("journal", "", "add", "hrs/log", "unrelated"); got == lost+"\n" {
		t.Fatal("add wrote to the lost page")
	}
	a.must("journal", "", "append", lost, "after", "the", "sync")
	history := a.must("journal", "", "history", lost)
	var id string
	for line := range strings.SplitSeq(history, "\n") {
		if _, d, ok := strings.Cut(line, "\t"); ok && strings.HasPrefix(d, "hrs/log/1/") {
			id = d[len("hrs/log/1/"):]
		}
	}
	if id == "" {
		t.Fatalf("the history of the lost page names no entry: %q", history)
	}
	a.must("journal", "", "fix", lost+"/"+id, "fixed", "after", "the", "sync")
	if got := a.must("journal", "", "read", lost); !strings.HasSuffix(got, "---\nlaptop text\nfixed after the sync\n") {
		t.Errorf("the lost page reads %q", got)
	}
	if got := a.must("journal", "", "history", lost); !strings.Contains(got, "\tfixes hrs/log/1/"+id) {
		t.Errorf("history does not say which entry the fix corrects: %q", got)
	}
	if got := a.must("journal", "", "read", "hrs/log/1"); strings.Contains(got, "sync") {
		t.Errorf("the live page took an entry of the lost page: %q", got)
	}

	a.must("journal", "", "delete", "hrs/log/1")
	if got := a.must("journal", "", "read", lost); !strings.Contains(got, "laptop text\n") {
		t.Errorf("a delete of the live page took the lost page: %q", got)
	}
	a.must("journal", "", "delete", lost)
	if _, err := a.run("journal", "", "read", lost); err == nil {
		t.Error("the deleted lost page still reads")
	}
	if got := a.must("journal", "", "ls", "--notebook", "hrs/log"); strings.Contains(got, "hrs/log/1") {
		t.Errorf("ls still lists page 1 after both deletes: %q", got)
	}
}

// A delete of the live page never removes the head of a lost page at its
// address. The lost page takes the address, ls never calls its head a
// leftover, and a search on the other device still finds it after a sync
// (Cato's test of #395 at a9459d8: following the hint of ls deleted the
// lost page for good).
func TestADeleteOfTheLivePageKeepsTheLostPage(t *testing.T) {
	a := newCitizen(t)
	b := a.device()
	a.must("journal", "passport\n", "write", "arc/loss/1", "--title", "Passport")
	b.env.now = a.env.Now().Add(time.Minute)
	b.must("journal", "groceries\n", "write", "arc/loss/1", "--title", "Groceries")
	a.syncWith(b)
	lost := lostAddress(a, "arc/loss/1")

	a.env.now = a.env.Now().Add(time.Hour)
	a.must("journal", "", "delete", "arc/loss/1")
	_, errs, err := a.runAll("", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errs, "no page names") || strings.Contains(errs, "journal delete") {
		t.Errorf("ls calls the lost head a leftover: %q", errs)
	}
	for _, address := range []string{"arc/loss/1", lost} {
		got := a.must("journal", "", "read", address)
		if !strings.HasSuffix(got, "---\npassport\n") {
			t.Errorf("%s reads %q, want the passport page", address, got)
		}
		// It moves up as it was written.
		if !strings.Contains(got, "created_at: 2026-09-29T12:00:00Z\n") {
			t.Errorf("%s reads %q, want the time the passport page was written", address, got)
		}
	}
	if got := a.must("journal", "", "history", "arc/loss/1"); !strings.Contains(got, "passport") {
		t.Errorf("the history of the page that moved up lost its head: %q", got)
	}
	// The delete that ls once named finds the page, not a leftover.
	if _, errs, err := a.runAll("", "delete", "arc/loss/1"); err != nil || strings.Contains(errs, "no page names") {
		t.Fatalf("a second delete gave %v, %q", err, errs)
	}

	// On two other devices, one delete and a sync: the phone finds the
	// passport.
	c := newCitizen(t)
	d := c.device()
	c.must("journal", "passport\n", "write", "arc/loss/1", "--title", "Passport")
	d.env.now = c.env.Now().Add(time.Minute)
	d.must("journal", "groceries\n", "write", "arc/loss/1", "--title", "Groceries")
	c.syncWith(d)
	c.must("journal", "", "delete", "arc/loss/1")
	c.syncWith(d)
	if got := d.must("journal", "", "search", "passport"); !strings.HasPrefix(got, "arc/loss/1\t") {
		t.Errorf("after the sync, the phone's search for passport shows %q", got)
	}
	if got := d.must("journal", "", "ls", "--notebook", "arc/loss"); strings.Contains(got, "Groceries") || !strings.Contains(got, "Passport") {
		t.Errorf("after the sync, the phone lists %q", got)
	}
}

// Each address that history prints works with fix, on a lost page too.
func TestFixTakesTheAddressesOfHistory(t *testing.T) {
	a := newCitizen(t)
	b := a.device()
	a.must("journal", "laptop\n", "new", "hrs/log", "--title", "Laptop")
	a.must("journal", "", "append", "hrs/log/1", "teh", "laptop", "entry")
	b.env.now = a.env.Now().Add(time.Minute)
	b.must("journal", "phone\n", "new", "hrs/log", "--title", "Phone")
	a.syncWith(b)
	lost := lostAddress(a, "hrs/log/1")
	for _, rec := range historyOf(a, lost) {
		d := rec["tags"].(map[string]any)["d"].(string)
		if !strings.HasPrefix(d, "hrs/log/1/") {
			continue
		}
		a.must("journal", "", "fix", d, "the", "laptop", "entry")
	}
	if got := a.must("journal", "", "read", lost); !strings.HasSuffix(got, "---\nlaptop\nthe laptop entry\n") {
		t.Errorf("the lost page reads %q after the fix", got)
	}
}

// A fix of the head of a lost page stays when the page moves up, on this
// device and on the other after a sync. Each fix that history lists works
// again with fix (#402, Cato's retest of #395 at aba6e42: after the move-up,
// read showed the old head, and a fix of the fix exited 0 and changed
// nothing).
func TestAHeadFixStaysWhenTheLostPageMovesUp(t *testing.T) {
	a := newCitizen(t)
	b := a.device()
	a.must("journal", "H\n", "write", "arc/loss/1", "--title", "Lost")
	b.env.now = a.env.Now().Add(time.Minute)
	b.must("journal", "winner\n", "write", "arc/loss/1", "--title", "Winner")
	a.syncWith(b)
	lost := lostAddress(a, "arc/loss/1")
	a.must("journal", "", "fix", lost, "head", "fixed", "while", "losing")
	if got := a.must("journal", "", "read", lost); !strings.HasSuffix(got, "---\nhead fixed while losing\n") {
		t.Fatalf("the lost page reads %q before the move-up", got)
	}

	a.env.now = a.env.Now().Add(time.Hour)
	a.must("journal", "", "delete", "arc/loss/1")
	a.syncWith(b)
	for name, c := range map[string]*citizen{"laptop": a, "phone": b} {
		for _, address := range []string{"arc/loss/1", lost} {
			if got := c.must("journal", "", "read", address); !strings.HasSuffix(got, "---\nhead fixed while losing\n") {
				t.Errorf("%s: %s reads %q after the move-up, want the head fix", name, address, got)
			}
		}
	}

	// A fix of the head fix, by the address that history lists for it.
	var fix string
	for _, rec := range historyOf(a, "arc/loss/1") {
		if d := rec["tags"].(map[string]any)["d"].(string); strings.HasPrefix(d, "arc/loss/1/") {
			fix = d
		}
	}
	if fix == "" {
		t.Fatal("history lists no fix of the head")
	}
	a.must("journal", "", "fix", fix, "head", "fixed", "again")
	if got := a.must("journal", "", "read", "arc/loss/1"); !strings.HasSuffix(got, "---\nhead fixed again\n") {
		t.Errorf("after a fix of the head fix, read %q", got)
	}
	// A newer fix at the number wins over the fixes at the lost address.
	a.env.now = a.env.Now().Add(time.Minute)
	a.must("journal", "", "fix", "arc/loss/1", "head", "fixed", "at", "the", "number")
	if got := a.must("journal", "", "read", lost); !strings.HasSuffix(got, "---\nhead fixed at the number\n") {
		t.Errorf("after a fix at the number, read %q", got)
	}
}

// tailOf runs journal tail on the address until the live events stop, and
// returns its output.
func tailOf(t *testing.T, c *citizen, address string, live []nostr.Event) string {
	t.Helper()
	c.env.live = make(chan nostr.Event, len(live)+1)
	done := make(chan string)
	go func() {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		Run(ctx, c.env, Installed{Manifest: specManifests(t)["journal"], Author: c.author, Name: "journal"}, []string{"tail", address},
			Stdio{In: strings.NewReader(""), Out: &out, Err: &bytes.Buffer{}})
		done <- out.String()
	}()
	time.Sleep(100 * time.Millisecond)
	for _, event := range live {
		c.env.live <- event
	}
	time.Sleep(300 * time.Millisecond)
	close(c.env.live)
	return <-done
}

// tail shows the head of a page before its entries, and the entries in time
// order, though they are of one second and come in any order (#420: a write
// and an append in one second came out of the store poll as the entry, then
// the page).
func TestTailShowsThePageBeforeItsEntries(t *testing.T) {
	want := pageHeader + "first note\nsecond note\nthird note\n"
	for range 10 {
		c := newCitizen(t)
		c.must("journal", pageHeader+"first note\n", "write", page)
		c.must("journal", "", "append", page, "second", "note")
		c.must("journal", "", "append", page, "third", "note")
		if got := tailOf(t, c, page, nil); got != want {
			t.Fatalf("tail of the stored page showed %q, want %q", got, want)
		}
	}
	// The same events, live, newest first.
	c := newCitizen(t)
	c.must("journal", pageHeader+"first note\n", "write", page)
	c.must("journal", "", "append", page, "second", "note")
	c.must("journal", "", "append", page, "third", "note")
	events := testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}}))
	slices.SortFunc(events, func(a, b nostr.Event) int { return strings.Compare(b.ID.Hex(), a.ID.Hex()) })
	fresh := c.device()
	if got := tailOf(t, fresh, page, events); got != want {
		t.Fatalf("tail of live events in any order showed %q, want %q", got, want)
	}
}
