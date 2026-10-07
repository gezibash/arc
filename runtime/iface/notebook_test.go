package iface

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/internal/testutil"
)

func writeNotebookPage(c *citizen, book string, page int, title, body string) {
	c.t.Helper()
	document := fmt.Sprintf("---\ntitle: %s\npage: %d\nnotebook: %s\n---\n%s", title, page, book, body)
	c.must("journal", document, "write", fmt.Sprintf("%s/%d", book, page))
}

func TestNotebookManagedTimestampsAndOrdinals(t *testing.T) {
	c := newCitizen(t)
	c.env.now = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	input := "---\ntitle: First\npage: 1\nnotebook: arc/research\ncreated_at: 2000-01-01T00:00:00Z\nupdated_at: 2000-01-01T00:00:00Z\n---\n# First\n"
	c.must("journal", input, "write", "arc/research/1")
	want := "---\ntitle: First\npage: 1\nnotebook: arc/research\ncreated_at: 2026-09-28T10:00:00Z\nupdated_at: 2026-09-28T10:00:00Z\n---\n# First\n"
	if got := c.must("journal", "", "read", "arc/research/1"); got != want {
		t.Fatal(got)
	}
	c.env.now = time.Date(2026, 9, 29, 9, 15, 0, 0, time.UTC)
	c.must("journal", "", "append", "arc/research/1", "later")
	want = "---\ntitle: First\npage: 1\nnotebook: arc/research\ncreated_at: 2026-09-28T10:00:00Z\nupdated_at: 2026-09-29T09:15:00Z\n---\n# First\nlater\n"
	if got := c.must("journal", "", "read", "arc/research/1"); got != want {
		t.Fatal(got)
	}
	c.env.now = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	c.must("journal", input, "write", "arc/research/1")
	got := c.must("journal", "", "read", "arc/research/1")
	if !strings.Contains(got, "created_at: 2026-09-28T10:00:00Z\nupdated_at: 2026-09-29T10:00:00Z") {
		t.Fatal(got)
	}
	for _, value := range []string{"0", "01", "-1", "chapter", "9223372036854775808"} {
		body := strings.Replace(input, "page: 1", "page: "+value, 1)
		r := &run{env: c.env, in: Installed{Manifest: specManifests(t)["journal"]}}
		if _, err := r.notebookHeader(&Publish{Kind: "page"}, body, nil); err == nil {
			t.Fatalf("header accepted page %q", value)
		}
		if _, err := c.run("journal", body, "write", "arc/research/"+value); err == nil {
			t.Fatalf("accepted page %q", value)
		}
	}
}

func TestNotebookChronologyFiltersAndNumericNavigation(t *testing.T) {
	c := newCitizen(t)
	c.env.now = time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC)
	writeNotebookPage(c, "arc/research", 10, "Tenth", "# Summary\nten\n")
	c.env.now = time.Date(2026, 9, 28, 23, 59, 59, 0, time.UTC)
	writeNotebookPage(c, "arc/research", 2, "Second result", "# Method\nsecond\n")
	c.env.now = time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	writeNotebookPage(c, "arc/research", 1, "First", "# Summary\none\n")
	writeNotebookPage(c, "arc/other", 2, "Other second", "# Other\nprivate to other notebook\n")
	listing := c.must("journal", "", "ls", "--notebook", "arc/research")
	lines := strings.Split(strings.TrimSpace(listing), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "arc/research/10\t") || !strings.HasPrefix(lines[1], "arc/research/2\t") || !strings.HasPrefix(lines[2], "arc/research/1\t") {
		t.Fatal(listing)
	}
	listing = c.must("journal", "", "ls", "--notebook", "arc/research", "--order", "page", "--reverse", "--limit", "2")
	lines = strings.Split(strings.TrimSpace(listing), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "arc/research/10\t") || !strings.HasPrefix(lines[1], "arc/research/2\t") {
		t.Fatal(listing)
	}
	listing = c.must("journal", "", "ls", "--notebook", "arc/research", "--from", "2026-09-28", "--to", "2026-09-28", "--title", "RESULT", "--page", "2")
	if strings.Count(listing, "\n") != 1 || !strings.HasPrefix(listing, "arc/research/2\tSecond result\t2026-09-28T23:59:59Z") {
		t.Fatal(listing)
	}
	if got := c.must("journal", "", "select", "--notebook", "arc/research", "--page", "2", "--section", "method"); got != "# Method\nsecond\n" {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "next", "arc/research/1"); !strings.Contains(got, "page: 2\n") || strings.Contains(got, "Other second") {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "prev", "arc/research/10"); !strings.Contains(got, "page: 2\n") {
		t.Fatal(got)
	}
	c.must("journal", "", "delete", "arc/research/2")
	if got := c.must("journal", "", "next", "arc/research/1"); !strings.Contains(got, "page: 10\n") {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "prev", "arc/research/10"); !strings.Contains(got, "page: 1\n") {
		t.Fatal(got)
	}
	for _, args := range [][]string{{"next", "arc/research/10"}, {"prev", "arc/research/1"}, {"ls", "--from", "2026-10-01", "--to", "2026-09-01"}, {"ls", "--from", "wrong"}, {"ls", "--limit", "0"}, {"ls", "--order", "wrong"}} {
		if _, err := c.run("journal", "", args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestNotebookIndexTocAndSectionReferences(t *testing.T) {
	c := newCitizen(t)
	body := "# Overview\nintro\n## Method\nsteps\n### Detail\nmore\n## Limits\nlimits\n```md\n# Not a heading\n```\n## Method\nrepeat\n"
	writeNotebookPage(c, "arc/research", 1, "Result", body)
	index := c.must("journal", "", "index", "arc/research")
	prefix := "journal+arc://" + c.env.Me().Hex() + "/arc/research/1"
	if !strings.Contains(index, "| 1 | Result | 2026-09-29T12:00:00Z | 2026-09-29T12:00:00Z |") || !strings.Contains(index, "## Index\n") || !strings.Contains(index, "## Table of contents\n") {
		t.Fatal(index)
	}
	toc := c.must("journal", "", "toc", "arc/research")
	for _, ref := range []string{prefix, prefix + "#overview", prefix + "#method", prefix + "#detail", prefix + "#limits", prefix + "#method-1"} {
		if !strings.Contains(toc, "("+ref+")") {
			t.Fatalf("missing reference %s: %s", ref, toc)
		}
	}
	if strings.Contains(toc, "Not a heading") {
		t.Fatal(toc)
	}
	if got := c.must("journal", "", "read", prefix+"#method"); got != "## Method\nsteps\n### Detail\nmore\n" {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "read", "arc/research/1#method-1"); got != "## Method\nrepeat\n" {
		t.Fatal(got)
	}
	if _, err := c.run("journal", "", "read", "journal+arc://"+strings.Repeat("0", 64)+"/arc/research/1"); err == nil {
		t.Fatal("accepted another identity's journal reference")
	}
	snapshots := 0
	for _, event := range testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}})) {
		if strings.Contains(event.Content, "Result") || strings.Contains(event.Tags.GetD(), "arc/research") {
			t.Fatal("index or page leaked plaintext")
		}
		opened, err := draft.Open(context.Background(), c.env.Keyer(), event)
		if err != nil {
			t.Fatal(err)
		}
		if opened.Event.Kind == 30023 && opened.Event.Tags.GetD() == "arc/research" {
			snapshots++
			if opened.Event.Content != index {
				t.Fatal("stored index differs from generated view")
			}
		}
	}
	if snapshots != 1 {
		t.Fatalf("stored index count %d", snapshots)
	}
	c.must("journal", "", "delete", "arc/research/1")
	if got := c.must("journal", "", "index", "arc/research"); strings.Contains(got, "Result") || strings.Contains(got, "#method") {
		t.Fatal(got)
	}
}

func TestNotebookSearchAppliesFiltersBeforeRankingLimit(t *testing.T) {
	c := newCitizen(t)
	writeNotebookPage(c, "arc/research", 1, "Old", "unrelated\n")
	writeNotebookPage(c, "arc/research", 2, "Relevant", "needle needle needle\n")
	writeNotebookPage(c, "arc/other", 1, "Other", "needle\n")
	got := c.must("journal", "", "search", "--notebook", "arc/research", "--limit", "1", "needle")
	if !strings.HasPrefix(got, "arc/research/2\t") || strings.Contains(got, "arc/other") || strings.Count(got, "\n") != 1 {
		t.Fatal(got)
	}
}

func TestMarkdownHeadingsAndSectionBounds(t *testing.T) {
	body := "Setext title\n============\nintro\n## Same\nfirst\n### Child\nnested\n## Same\nsecond\n~~~md\n## Fake\n~~~\n    # Indented code\n# Last\nend\n"
	headings := markdownHeadings(body)
	want := []string{"setext-title", "same", "child", "same-1", "last"}
	if len(headings) != len(want) {
		t.Fatalf("headings %+v", headings)
	}
	for i, h := range headings {
		if h.anchor != want[i] {
			t.Fatalf("anchor %d: %s", i, h.anchor)
		}
	}
	section, err := markdownSection(body, "same")
	if err != nil || section != "## Same\nfirst\n### Child\nnested\n" {
		t.Fatalf("section %q %v", section, err)
	}
	if _, err = markdownSection(body, "missing"); err == nil {
		t.Fatal("missing section accepted")
	}
}

func TestNotebookSelectionSkipsMissingPartsOutsideItsFilters(t *testing.T) {
	c := newCitizen(t)
	writeNotebookPage(c, "arc/other", 1, "Other", strings.Repeat("x", 2*draft.PartSize))
	parts := testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.PartKind}}))
	if len(parts) == 0 {
		t.Fatal("expected a multipart page")
	}
	request := nostr.Event{Kind: 5, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"e", parts[0].ID.Hex()}}}
	request.Sign(c.env.me)
	if _, err := c.env.store.Save(request); err != nil {
		t.Fatal(err)
	}
	// This also tests the automatic index refresh after a write.
	writeNotebookPage(c, "arc/research", 1, "Available", "# Method\nneedle\n")
	for _, args := range [][]string{{"search", "--notebook", "arc/research", "needle"}, {"index", "arc/research"}, {"toc", "arc/research"}} {
		got := c.must("journal", "", args...)
		if !strings.Contains(got, "Available") || strings.Contains(got, "arc/other") {
			t.Fatal(got)
		}
	}
	if _, err := c.run("journal", "", "read", "arc/other/1"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing source part was not reported: %v", err)
	}
}

func TestNotebookIndexRepairsStaleSnapshots(t *testing.T) {
	c := newCitizen(t)
	writeNotebookPage(c, "arc/research", 1, "Current", "# Current heading\ncurrent\n")
	r := &run{ctx: context.Background(), env: c.env, in: Installed{Manifest: specManifests(t)["journal"], Author: c.author, Name: "journal"}}
	if err := r.saveNotebookIndex("page", "arc/research", "# Obsolete snapshot\n"); err != nil {
		t.Fatal(err)
	}
	// ToC is derived from pages even before a stored index has been repaired.
	if got := c.must("journal", "", "toc", "arc/research"); !strings.Contains(got, "Current heading") || strings.Contains(got, "Obsolete") {
		t.Fatal(got)
	}
	index := c.must("journal", "", "index", "arc/research")
	if !strings.Contains(index, "| 1 | Current |") || strings.Contains(index, "Obsolete") {
		t.Fatal(index)
	}
	d, err := r.keyed("index", "arc/research")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := r.current(d)
	if err != nil || stored == nil || stored.Event.Content != index {
		t.Fatalf("index was not repaired: %v", err)
	}
}

// A write must cost the same in a full notebook as in an empty one. Before,
// each write stored the whole notebook index again, so 1,000 pages of 2 KB
// filled 237 MB.
func TestNotebookWriteCostDoesNotGrowWithTheNotebook(t *testing.T) {
	c := newCitizen(t)
	stored := func() int {
		events, err := c.env.store.Query(nostr.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, e := range events {
			total += len(e.String())
		}
		return total
	}
	body := strings.Repeat("# Heading\nsome notes about the run\n", 8)
	write := func(n int) int {
		before := stored()
		writeNotebookPage(c, "arc/research", n, fmt.Sprintf("Page %d", n), body)
		return stored() - before
	}

	first := write(1)
	for n := 2; n < 60; n++ {
		write(n)
	}
	last := write(60)
	if last > first*3/2 {
		t.Errorf("the 60th write stored %d bytes, and the first stored %d", last, first)
	}
	if got := c.must("journal", "", "toc", "arc/research"); !strings.Contains(got, "60. Page 60") {
		t.Errorf("the table of contents lacks page 60:\n%s", got)
	}
}

func TestNotebookTailTreatsTimestampChangesAsAppend(t *testing.T) {
	m := specManifests(t)["journal"]
	r := &run{in: Installed{Manifest: m}}
	header := "---\ntitle: Notes\npage: 1\nnotebook: arc/research\ncreated_at: 2026-09-28T12:00:00Z\nupdated_at: 2026-09-28T12:00:00Z\n---\n"
	feed := func(text string) []*entry {
		return r.tail([]*entry{{d: "one", rec: Record{"kind": "page", "text": text}}})
	}
	feed(header + "first\n")
	later := strings.Replace(header, "updated_at: 2026-09-28T12:00:00Z", "updated_at: 2026-09-29T12:00:00Z", 1)
	got := feed(later + "first\nsecond\n")
	if len(got) != 1 || got[0].rec["text"] != "second\n" {
		t.Fatalf("append tail %+v", got)
	}
	if got := feed(strings.Replace(later, "2026-09-29T12:00:00Z", "2026-09-29T13:00:00Z", 1) + "first\nsecond\n"); len(got) != 0 {
		t.Fatal("timestamp-only change was emitted")
	}
	renamed := strings.Replace(later, "title: Notes", "title: Renamed", 1) + "first\nsecond\n"
	got = feed(renamed)
	if len(got) != 1 || got[0].rec["text"] != "--- rewritten ---\n"+renamed {
		t.Fatal("metadata change was not shown as a rewrite")
	}
}

func TestNotebookTimestampGrowthKeepsMultipartContentWithinBounds(t *testing.T) {
	c := newCitizen(t)
	header := "---\ntitle: Full piece\npage: 1\nnotebook: arc/research\ncreated_at: 2026-09-29T12:00:00Z\nupdated_at: 2026-09-29T12:00:00Z\n---\n"
	body := header + strings.Repeat("x", draft.PartSize-len(header)-1) + "\n" + strings.Repeat("y\n", draft.PartSize) + "last\n"
	c.must("journal", body, "write", "arc/research/1")
	c.env.now = time.Date(2026, 9, 29, 13, 0, 0, 123456789, time.UTC)
	c.must("journal", "", "append", "arc/research/1", "appended")
	want := strings.Replace(body, "updated_at: 2026-09-29T12:00:00Z", "updated_at: 2026-09-29T13:00:00.123456789Z", 1) + "appended\n"
	if got := c.must("journal", "", "read", "arc/research/1"); got != want {
		t.Fatal("timestamp update lost or reordered content")
	}
	wraps := testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"k": {"30023"}}}))
	var opened draft.Draft
	for _, wrap := range wraps {
		value, err := draft.Open(context.Background(), c.env.Keyer(), wrap)
		if err != nil {
			t.Fatal(err)
		}
		if value.Event.Tags.GetD() == "arc/research/1" {
			opened = value
		}
	}
	if len(opened.Event.Content) > draft.PartSize {
		t.Fatalf("first piece exceeds protocol bound: %d", len(opened.Event.Content))
	}
	for _, id := range opened.Parts() {
		partID, err := nostr.IDFromHex(id)
		if err != nil {
			t.Fatal(err)
		}
		parts := testutil.Must(c.env.store.Query(nostr.Filter{IDs: []nostr.ID{partID}}))
		text, err := draft.OpenPart(context.Background(), c.env.Keyer(), parts[0])
		if err != nil || len(text) > draft.PartSize {
			t.Fatalf("part exceeds protocol bound: %d %v", len(text), err)
		}
	}
}

func TestNotebookIndexesDoNotEnterKpiQueries(t *testing.T) {
	c := newCitizen(t)
	writeNotebookPage(c, "arc/research", 1, "Research", "# Decisions\nnotes\n")
	c.must("journal", "", "kpi", "set", "arc/research", "auc", "0.87")
	got := c.must("journal", "", "kpi", "latest", "arc/research")
	if !strings.Contains(got, "\tauc\t0.87\t") || strings.Contains(got, "Decisions") || strings.Count(got, "\n") != 1 {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "index", "arc/research"); !strings.Contains(got, "Research") || strings.Contains(got, "auc") {
		t.Fatal(got)
	}
}

func TestNotebookBleveSearchTracksEditsAndDeletion(t *testing.T) {
	c := newCitizen(t)
	writeNotebookPage(c, "arc/research", 1, "Radio", "blue birds fly over radio\n")
	writeNotebookPage(c, "arc/research", 2, "Cable", "blue cables carry birds\n")
	writeNotebookPage(c, "arc/other", 1, "Other", "blue birds radio\n")
	for _, tc := range []struct {
		query string
		want  string
	}{
		{`"blue birds"`, "arc/research/1"}, {"+birds -radio", "arc/research/2"}, {"title:radio", "arc/research/1"},
	} {
		got := c.must("journal", "", "search", "--syntax", "--notebook", "arc/research", tc.query)
		if !strings.HasPrefix(got, tc.want+"\t") || strings.Count(got, "\n") != 1 {
			t.Fatalf("%s: %s", tc.query, got)
		}
	}
	c.must("journal", "", "append", "arc/research/2", "sqlite")
	if got := c.must("journal", "", "search", "--notebook", "arc/research", "sqlite"); !strings.HasPrefix(got, "arc/research/2\t") {
		t.Fatal(got)
	}
	writeNotebookPage(c, "arc/research", 1, "Changed", "turtles swim\n")
	if got := c.must("journal", "", "search", "--notebook", "arc/research", "radio"); got != "no results\n" {
		t.Fatal(got)
	}
	c.must("journal", "", "delete", "arc/research/2")
	if got := c.must("journal", "", "search", "--notebook", "arc/research", "sqlite"); got != "no results\n" {
		t.Fatal(got)
	}
	if _, err := c.run("journal", "", "search", "--syntax", `"unclosed`); err == nil {
		t.Fatal("invalid search syntax accepted")
	}
}

func TestNotebookBleveReusesUnchangedMultipartBodies(t *testing.T) {
	c := newCitizen(t)
	writeNotebookPage(c, "arc/research", 1, "Large", strings.Repeat("padding ", draft.PartSize/4)+"needle\n")
	c.must("journal", "", "search", "needle")
	c.env.fetchedIDs = 0
	if got := c.must("journal", "", "search", "needle"); !strings.HasPrefix(got, "arc/research/1\t") {
		t.Fatal(got)
	}
	if c.env.fetchedIDs != 0 {
		t.Fatalf("unchanged search refetched %d parts", c.env.fetchedIDs)
	}
	c.must("journal", "", "append", "arc/research/1", "sqlite")
	c.env.fetchedIDs = 0
	if got := c.must("journal", "", "search", "sqlite"); !strings.HasPrefix(got, "arc/research/1\t") || c.env.fetchedIDs == 0 {
		t.Fatalf("changed page not refreshed: %s, part loads %d", got, c.env.fetchedIDs)
	}
}

func TestNotebookSearchJSONKeepsCompleteMarkdown(t *testing.T) {
	c := newCitizen(t)
	body := strings.Repeat("source line\n", draft.PartSize/4) + "needle\n"
	c.must("journal", pageHeader+body, "write", page)
	c.must("journal", "", "search", "needle") // Warm the index first.
	output := c.must("journal", "", "search", "--json", "needle")
	var record Record
	if err := json.Unmarshal([]byte(output), &record); err != nil {
		t.Fatal(err)
	}
	if record["text"] != pageHeader+body {
		t.Fatal("JSON search lost the source Markdown")
	}
}

// The runtime uses the installed app's namespace, so another notebook app can
// compose the same primitives without depending on Journal's name.
func TestNotebookCommandsUseTheirOwnAppReferences(t *testing.T) {
	c := newCitizen(t)
	m := specManifests(t)["journal"]
	m.ID = "lab-notes"
	run := func(input string, words ...string) (string, error) {
		var out bytes.Buffer
		err := Run(context.Background(), c.env, Installed{Manifest: m, Author: c.author, Name: "lab-notes"}, words,
			Stdio{In: strings.NewReader(input), Out: &out})
		return out.String(), err
	}
	document := "---\ntitle: Sample\npage: 1\nnotebook: lab/notes\n---\n# Result\nMeasured output.\n"
	if _, err := run(document, "write", "lab/notes/1"); err != nil {
		t.Fatal(err)
	}
	reference := "lab-notes+arc://" + c.env.Me().Hex() + "/lab/notes/1#result"
	toc, err := run("", "toc", "lab/notes")
	if err != nil || !strings.Contains(toc, "("+reference+")") || strings.Contains(toc, "journal+arc://") {
		t.Fatalf("toc: %q, error: %v", toc, err)
	}
	if got, err := run("", "read", reference); err != nil || got != "# Result\nMeasured output.\n" {
		t.Fatalf("read reference: %q, error: %v", got, err)
	}
}
