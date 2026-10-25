package iface

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/internal/testutil"
)

func TestJournalFrontmatterRejectsInvalidWritesWithoutStoring(t *testing.T) {
	cases := map[string]string{
		"duplicate":      "---\ntitle: One\ntitle: Two\n---\n",
		"unclosed":       "---\ntitle: Notes\n",
		"wrong notebook": "---\ntitle: Notes\npage: 1\nnotebook: hrs/status\n---\n",
		"wrong page":     "---\ntitle: Notes\npage: other\nnotebook: hrs/ablations\n---\n",
		"list":           "---\ntitle: [One, Two]\n---\n",
		"multiline":      "---\ntitle: \"One\\nTwo\"\n---\n",
		"malformed":      "---\ntitle Notes\n---\n",
		"large header":   "---\ntitle: " + strings.Repeat("x", 4096) + "\n---\n",
		// A page with no text and no title is no page.
		"quoted empty":  "---\ntitle: \"\"\n---\n",
		"missing title": "---\ntype: decision\npage: 1\nnotebook: hrs/ablations\n---\n",
		"no text":       "",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCitizen(t)
			for _, flags := range [][]string{nil, {"--dry-run"}} {
				_, err := c.run("journal", body, append([]string{"write", page}, flags...)...)
				if err == nil || !strings.Contains(err.Error(), "frontmatter") && !strings.Contains(err.Error(), "no text") {
					t.Fatalf("accepted invalid header: %v", err)
				}
			}
			if events := testutil.Must(c.env.store.Query(nostr.Filter{})); len(events) != 0 {
				t.Fatalf("invalid write stored %d events", len(events))
			}
		})
	}
}

// A write fills in the front matter that the text does not give: notebook
// and page from the address, and the title from --title, else from the first
// heading, else from the date (proposal journal-v2, section 4.3; Cato's
// finding 2 of 2026-10-19: --title did not work).
func TestAWriteFillsInTheFrontMatter(t *testing.T) {
	stamps := "created_at: 2026-09-29T12:00:00.000Z\nupdated_at: 2026-09-29T12:00:00.000Z\n---\n"
	for name, tc := range map[string]struct {
		in    string
		flags []string
		want  string
	}{
		"title flag":   {"plain text\n", []string{"--title", "From the flag"}, "---\ntitle: From the flag\nnotebook: hrs/ablations\npage: 1\n" + stamps + "plain text\n"},
		"heading":      {"# Decisions\nbody\n", nil, "---\ntitle: Decisions\nnotebook: hrs/ablations\npage: 1\n" + stamps + "# Decisions\nbody\n"},
		"date":         {"no heading\n", nil, "---\ntitle: 2026-09-29\nnotebook: hrs/ablations\npage: 1\n" + stamps + "no heading\n"},
		"partial":      {"---\ntitle: Notes\ntype: decision\n---\nbody\n", nil, "---\ntitle: Notes\ntype: decision\nnotebook: hrs/ablations\npage: 1\n" + stamps + "body\n"},
		"empty title":  {"---\ntitle: \n---\nNotes\n", nil, "---\ntitle: 2026-09-29\nnotebook: hrs/ablations\npage: 1\n" + stamps + "Notes\n"},
		"quoted title": {"text\n", []string{"--title", "\"Quoted\" start"}, "---\ntitle: \"\\\"Quoted\\\" start\"\nnotebook: hrs/ablations\npage: 1\n" + stamps + "text\n"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCitizen(t)
			c.must("journal", tc.in, append([]string{"write", page}, tc.flags...)...)
			if got := c.must("journal", "", "read", page); got != tc.want {
				t.Errorf("read\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestJournalFrontmatterTitleAppendAndFailedReplacement(t *testing.T) {
	c := newCitizen(t)
	body := "---\r\ntitle: \"Memory: decisions\"\r\ntype: decision\r\npage: 1\r\nnotebook: hrs/ablations\r\n---\r\n# Decisions\n"
	c.must("journal", body, "write", page)
	if got := c.must("journal", "", "ls"); !strings.Contains(got, page+"\tMemory: decisions\t") {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "search", "Memory"); !strings.HasPrefix(got, page+"\t") {
		t.Fatal(got)
	}
	c.must("journal", "", "append", page, "next")
	want := "---\ntitle: \"Memory: decisions\"\ntype: decision\npage: 1\nnotebook: hrs/ablations\ncreated_at: 2026-09-29T12:00:00.000Z\nupdated_at: 2026-09-29T12:00:00.000Z\n---\n# Decisions\nnext\n"
	if got := c.must("journal", "", "read", page); got != want {
		t.Fatalf("roundtrip %q", got)
	}
	for _, words := range [][]string{{"write", page}, {"write", page, "--title", "Different"}} {
		input := "no header\n"
		if len(words) > 2 {
			input = body
		}
		if _, err := c.run("journal", input, words...); err == nil {
			t.Fatal("invalid replacement accepted")
		}
		if got := c.must("journal", "", "read", page); got != want {
			t.Fatal("invalid replacement changed stored page")
		}
	}
	if _, err := c.run("journal", "", "append", "arc/status/missing", "no header"); err == nil {
		t.Fatal("append created a headerless page")
	}
}

// A page that an older arc wrote with no header stays readable, takes
// entries, and is never written again.
func TestJournalFrontmatterHistoricalPagesStayReadable(t *testing.T) {
	c := newCitizen(t)
	old := specManifests(t)["journal"]
	kind := old.Kinds["page"]
	kind.Frontmatter, kind.NotebookIndex, kind.NotebookEntry = nil, "", ""
	old.Kinds["page"] = kind
	for i := range old.Commands {
		if old.Commands[i].Path[0] == "write" {
			old.Commands[i].Action = Action{Publish: &Publish{Kind: "page", D: "{{address|keyed:page}}", Revise: "replace", Content: Content{Text: "{{body}}"}, Tags: [][]string{{"d", "{{address}}"}}}}
		}
	}
	var out bytes.Buffer
	err := Run(context.Background(), c.env, Installed{Manifest: old, Author: c.author, Name: "journal"}, []string{"write", page}, Stdio{In: strings.NewReader("legacy body\n"), Out: &out, Err: &out})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.must("journal", "", "read", page); got != "legacy body\n" {
		t.Fatal(got)
	}
	// An entry leaves the head as it was, so it needs no header.
	c.must("journal", "", "append", page, "new text")
	if got := c.must("journal", "", "read", page); got != "legacy body\nnew text\n" {
		t.Fatal(got)
	}
	if _, err := c.run("journal", "---\ntitle: Legacy\npage: 1\nnotebook: hrs/ablations\n---\nlegacy body\n", "write", page); err == nil {
		t.Fatal("a write replaced a page that exists")
	}
}

func TestManifestFrontmatterRequiredFields(t *testing.T) {
	for _, fields := range [][]string{{"Title"}, {"title", "title"}, {""}} {
		manifest := specManifests(t)["journal"]
		kind := manifest.Kinds["page"]
		kind.Frontmatter = fields
		manifest.Kinds["page"] = kind
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Parse(data); err == nil {
			t.Fatalf("accepted invalid required fields %v", fields)
		}
	}
}

func TestFrontmatterSeparatesQuotedFieldsFromMarkdown(t *testing.T) {
	header := "---\ntitle: 'A ''quoted'' title'\ntype: decision\n---\n# Body\n"
	fields, body, err := frontmatter(header)
	if err != nil || fields["title"] != "A 'quoted' title" || fields["type"] != "decision" || body != "# Body\n" {
		t.Fatalf("fields=%v body=%q err=%v", fields, body, err)
	}
	fields, body, err = frontmatter("# Legacy\n")
	if err != nil || len(fields) != 0 || body != "# Legacy\n" {
		t.Fatalf("legacy fields=%v body=%q err=%v", fields, body, err)
	}
}

func TestJournalFrontmatterSeparatesNotebooks(t *testing.T) {
	c := newCitizen(t)
	first := "---\ntitle: Architecture decision\npage: 1\nnotebook: arc/architecture\n---\nCore owns sessions.\n"
	second := "---\ntitle: Session status\npage: 1\nnotebook: arc/status\n---\nProviders migrated.\n"
	c.must("journal", first, "write", "arc/architecture/1")
	c.must("journal", second, "write", "arc/status/1")
	if got := c.must("journal", "", "read", "arc/architecture/1"); got != strings.Replace(first, "\n---\n", "\ncreated_at: 2026-09-29T12:00:00.000Z\nupdated_at: 2026-09-29T12:00:00.000Z\n---\n", 1) {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "read", "arc/status/1"); got != strings.Replace(second, "\n---\n", "\ncreated_at: 2026-09-29T12:00:00.000Z\nupdated_at: 2026-09-29T12:00:00.000Z\n---\n", 1) {
		t.Fatal(got)
	}
	if got := c.must("journal", "", "ls", "arc/architecture/"); !strings.HasPrefix(got, "arc/architecture/1\tArchitecture decision\t") || strings.Contains(got, "arc/status/") {
		t.Fatal(got)
	}
}

func TestFrontmatterRejectsPlainAndPrivatePublish(t *testing.T) {
	for _, visibility := range []string{"public", "private"} {
		t.Run(visibility, func(t *testing.T) {
			c := newCitizen(t)
			m := &Manifest{Interface: 1, ID: "notes", Shape: "data", Kinds: map[string]Kind{"note": {Kind: 1, Visibility: visibility, Frontmatter: []string{"title"}}}, Commands: []Command{{Path: []string{"write"}, Args: []Arg{{Name: "body", Kind: "positional", Type: "stdin"}}, Action: Action{Publish: &Publish{Kind: "note", Content: Content{Text: "{{body}}"}}}}}}
			if visibility == "private" {
				m.Commands[0].Action.Publish.To = []string{"{{me}}"}
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			m, err = Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = Run(context.Background(), c.env, Installed{Manifest: m, Author: c.author, Name: "notes"}, []string{"write"}, Stdio{In: strings.NewReader("no header\n"), Out: &out, Err: &out})
			if err == nil || !strings.Contains(err.Error(), "frontmatter") {
				t.Fatalf("publish bypassed policy: %v", err)
			}
			if len(c.env.net.inbox) != 0 || len(c.env.net.relays) != 0 || len(testutil.Must(c.env.store.Query(nostr.Filter{}))) != 0 {
				t.Fatal("invalid content escaped before validation")
			}
		})
	}
}
