package main

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

const (
	alice = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bob   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func testJournal(t *testing.T) *server {
	t.Helper()

	root := t.TempDir()
	held := &store{root: root}
	if err := held.ensure(); err != nil {
		t.Fatal(err)
	}

	return &server{
		store:  held,
		index:  &index{root: root, log: os.Stderr},
		pusher: &pusher{store: held, log: os.Stderr},
	}
}

func run(t *testing.T, s *server, from, message string) string {
	t.Helper()

	out, err := s.run(from, message)
	if err != nil {
		t.Fatalf("%q: %v", message, err)
	}
	return out
}

func fails(t *testing.T, s *server, from, message, want string) {
	t.Helper()

	_, err := s.run(from, message)
	if err == nil {
		t.Fatalf("%q passed", message)
	}
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("%q gave %q, want %q", message, err, want)
	}
}

// rev reads the revision out of a reply.
func rev(reply string) string {
	line := strings.SplitN(reply, "\n", 2)[0]
	return strings.TrimPrefix(line, "rev: ")
}

func TestWriteReadAndList(t *testing.T) {
	s := testJournal(t)

	written := run(t, s, alice, `write hrs/ablations/lr --title "LR sweep" --tags a,b`+"\n# LR sweep\n\nTried 3e-4.\n")
	if !strings.HasPrefix(written, "rev: ") {
		t.Fatalf("write = %q", written)
	}

	read := run(t, s, alice, "read hrs/ablations/lr")
	for _, want := range []string{"title: \"LR sweep\"", "author: " + alice, "# LR sweep", "Tried 3e-4."} {
		if !strings.Contains(read, want) {
			t.Errorf("read does not hold %q:\n%s", want, read)
		}
	}

	if got := run(t, s, alice, "ls"); got != "hrs" {
		t.Errorf("ls = %q", got)
	}
	if got := run(t, s, alice, "ls hrs"); got != "hrs/ablations" {
		t.Errorf("ls hrs = %q", got)
	}
	if got := run(t, s, alice, "ls hrs/ablations"); got != "hrs/ablations/lr\tLR sweep" {
		t.Errorf("ls hrs/ablations = %q", got)
	}
}

func TestAWriteThatMovedIsRefused(t *testing.T) {
	s := testJournal(t)

	first := rev(run(t, s, alice, "write hrs/a/p --body \"one\""))
	run(t, s, alice, "write hrs/a/p --body \"two\"")

	fails(t, s, alice, `write hrs/a/p --if-rev `+first+` --body "three"`, "conflict")

	current := rev(run(t, s, alice, "read hrs/a/p"))
	if _, err := s.run(alice, `write hrs/a/p --if-rev `+current+` --body "three"`); err != nil {
		t.Errorf("a write at the current revision failed: %v", err)
	}
}

func TestAWriteNeedsOneBody(t *testing.T) {
	s := testJournal(t)

	fails(t, s, alice, "write hrs/a/p", "missing body")
	fails(t, s, alice, "write hrs/a/p --body \"one\"\ntwo", "invalid_arguments")
}

// The body keeps its newlines, its quotes, and a word that looks like a flag.
func TestABodyKeepsWhatItHolds(t *testing.T) {
	s := testJournal(t)

	body := "line one\nline two --not-a-flag \"quoted\"\n"
	run(t, s, alice, `write hrs/a/p --body "line one\nline two --not-a-flag \"quoted\""`)

	read := run(t, s, alice, "read hrs/a/p")
	if !strings.HasSuffix(read, body) {
		t.Errorf("read = %q, want it to end with %q", read, body)
	}
}

func TestTheProjectBelongsToItsCreator(t *testing.T) {
	s := testJournal(t)
	run(t, s, alice, "write hrs/a/p --body \"one\"")

	fails(t, s, bob, "read hrs/a/p", "forbidden")
	fails(t, s, bob, "write hrs/a/p --body \"two\"", "forbidden")
	fails(t, s, bob, "acl hrs add "+bob, "forbidden")

	if got := run(t, s, alice, "acl hrs ls"); got != alice {
		t.Errorf("the list holds %q", got)
	}

	run(t, s, alice, "acl hrs add "+bob)
	if _, err := s.run(bob, "read hrs/a/p"); err != nil {
		t.Errorf("the citizen that was added cannot read: %v", err)
	}

	// The owner never leaves the list.
	run(t, s, alice, "acl hrs rm "+alice)
	if got := run(t, s, alice, "acl hrs ls"); !strings.HasPrefix(got, alice) {
		t.Errorf("the owner left the list: %q", got)
	}

	run(t, s, alice, "acl hrs rm "+bob)
	fails(t, s, bob, "read hrs/a/p", "forbidden")

	fails(t, s, alice, "acl hrs add short", "invalid_pubkey")
	fails(t, s, alice, "acl missing ls", "forbidden")
}

func TestAppendEditAndHistory(t *testing.T) {
	s := testJournal(t)
	run(t, s, alice, "write hrs/a/p --body \"first\"")

	// The text of an append ends at the first flag, so a word that looks
	// like one travels as a JSON string, as the CLI writes it.
	run(t, s, alice, `append hrs/a/p "a line with --dashes inside\nand another"`)
	read := run(t, s, alice, "read hrs/a/p")
	if !strings.Contains(read, "first\n\na line with --dashes inside\nand another\n") {
		t.Errorf("read = %q", read)
	}

	current := rev(read)
	run(t, s, alice, `edit hrs/a/p --if-rev `+current+` --find first --replace second`)
	if read := run(t, s, alice, "read hrs/a/p"); !strings.Contains(read, "second") {
		t.Errorf("the edit did not land: %q", read)
	}

	fails(t, s, alice, "edit hrs/a/p --find x", "missing --if-rev")
	fails(t, s, alice, "edit hrs/a/p --if-rev "+rev(run(t, s, alice, "read hrs/a/p")), "missing --find")

	history := run(t, s, alice, "history hrs/a/p")
	if len(strings.Split(history, "\n")) != 3 {
		t.Errorf("the history holds %d lines:\n%s", len(strings.Split(history, "\n")), history)
	}
	fails(t, s, alice, "history hrs/a/missing", "not_found")
}

func TestTheNumbersOfANotebook(t *testing.T) {
	s := testJournal(t)
	run(t, s, alice, "write hrs/a/p --body \"one\"")

	out := run(t, s, alice, "kpi set hrs/a auc 0.871 --ref df62851")
	if !strings.Contains(out, "auc=0.871") || !strings.Contains(out, "ref=df62851") {
		t.Errorf("kpi set = %q", out)
	}

	run(t, s, alice, "kpi set hrs/a auc 0.9")
	run(t, s, alice, "kpi set hrs/a steps 1000")

	log := run(t, s, alice, "kpi log hrs/a auc")
	if len(strings.Split(log, "\n")) != 2 {
		t.Errorf("the log holds %q", log)
	}

	latest := run(t, s, alice, "kpi latest hrs/a")
	rows := strings.Split(latest, "\n")
	if len(rows) != 2 || !strings.Contains(rows[0], "auc=0.9") || !strings.Contains(rows[1], "steps=1000") {
		t.Errorf("the latest hold %q", latest)
	}

	fails(t, s, alice, "kpi set hrs/a auc not-a-number", "invalid_number")
	if got := run(t, s, alice, "kpi latest hrs/missing"); got != "no records" {
		t.Errorf("a notebook without numbers gave %q", got)
	}
}

// A page shows the latest measurement where it marks one.
func TestAPageShowsTheNumbers(t *testing.T) {
	s := testJournal(t)

	run(t, s, alice, "write hrs/a/p --body \"auc: <!-- kpi: auc --> end\"")
	run(t, s, alice, "kpi set hrs/a auc 0.871")

	read := run(t, s, alice, "read hrs/a/p")
	if !strings.Contains(read, "auc = 0.871 (") {
		t.Errorf("read = %q", read)
	}

	// A marker without a measurement stays as it is.
	run(t, s, alice, "write hrs/a/q --body \"loss: <!-- kpi: loss --> end\"")
	if read := run(t, s, alice, "read hrs/a/q"); !strings.Contains(read, "<!-- kpi: loss -->") {
		t.Errorf("the marker went: %q", read)
	}
}

func TestAttachFetchAndLink(t *testing.T) {
	s := testJournal(t)
	run(t, s, alice, "write hrs/a/p --body \"one\"")

	data := []byte("the file bytes")
	encoded := base64.StdEncoding.EncodeToString(data)

	out := run(t, s, alice, "attach hrs/a/p --name plan.md --base64 "+encoded)
	sha := strings.TrimPrefix(strings.Split(out, "\n")[1], "sha256: ")

	if got := run(t, s, alice, "fetch "+sha); got != encoded {
		t.Errorf("fetch = %q", got)
	}

	read := run(t, s, alice, "read hrs/a/p")
	if !strings.Contains(read, "attachments:") || !strings.Contains(read, "name: plan.md") {
		t.Errorf("the page does not name the attachment:\n%s", read)
	}
	if !strings.Contains(read, "bytes: 14") {
		t.Errorf("the page does not hold the size:\n%s", read)
	}

	run(t, s, alice, "link hrs/a/p https://example.com/paper --name paper")
	read = run(t, s, alice, "read hrs/a/p")
	if !strings.Contains(read, "links:") || !strings.Contains(read, "uri: https://example.com/paper") {
		t.Errorf("the page does not hold the link:\n%s", read)
	}

	// A file on the machine of the caller names that caller.
	run(t, s, alice, "link hrs/a/p file:///tmp/data.csv")
	if read := run(t, s, alice, "read hrs/a/p"); !strings.Contains(read, "host: "+alice) {
		t.Errorf("the link does not name the host:\n%s", read)
	}

	fails(t, s, alice, "link hrs/a/p not-a-uri", "invalid_uri")
	fails(t, s, alice, "attach hrs/a/p --base64 "+encoded, "missing --name")
	fails(t, s, alice, "attach hrs/a/p --name x --base64 not-base64!", "invalid_base64")
	fails(t, s, alice, "fetch short", "not_found")
}

func TestReadsARangeOfLines(t *testing.T) {
	s := testJournal(t)
	run(t, s, alice, `write hrs/a/p --body "one\ntwo\nthree\nfour"`)

	// The range counts the lines of the page, and the revision stands above
	// them.
	got := run(t, s, alice, "read hrs/a/p --lines 6:7")
	if got != "rev: "+rev(got)+"\none\ntwo" {
		t.Errorf("the range gave %q", got)
	}
}

func TestRefusesAnAddressThatIsNotOne(t *testing.T) {
	s := testJournal(t)

	for _, message := range []string{
		"read hrs",
		"read hrs/a",
		"read hrs/a/p/q",
		"read HRS/a/p",
		"read ../a/p",
		"write hrs/a --body x",
		"kpi set hrs auc 1",
	} {
		fails(t, s, alice, message, "invalid_address")
	}

	fails(t, s, "", "ls", "forbidden")
	fails(t, s, alice, "nonsense", "unknown_command")

	if got := run(t, s, alice, "help"); !strings.HasPrefix(got, "journal commands") {
		t.Errorf("help = %q", got)
	}
}

// The frontmatter is written by the journal itself, so a page reads the same
// on every machine.
func TestTheFrontmatterHoldsItsOrder(t *testing.T) {
	s := testJournal(t)
	run(t, s, alice, `write hrs/a/p --title "A title" --tags one,two --body "the body"`)

	read := run(t, s, alice, "read hrs/a/p")
	body := strings.SplitN(read, "\n", 2)[1]

	if !strings.HasPrefix(body, "---\ntitle: \"A title\"\ncreated: ") {
		t.Errorf("the frontmatter starts %q", body[:60])
	}

	order := []string{"title:", "created:", "author:", "updated:", "tags:"}
	at := 0
	for _, key := range order {
		index := strings.Index(body, key)
		if index < at {
			t.Errorf("%s stands out of order", key)
		}
		at = index
	}

	if !strings.Contains(body, "tags:\n  - one\n  - two\n") {
		t.Errorf("the tags are %q", body)
	}
}
