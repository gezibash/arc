package toolbox_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/sealedbox"
	"github.com/gezibash/arc/toolbox"
)

// thread writes the answer of a thread command: a title, then one record
// for each message.
func thread(records ...string) string {
	return "thread with rose\n" + strings.Join(records, "\n")
}

func TestOpensSealedTokens(t *testing.T) {
	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := sealedbox.SealTo(me.PublicKey, []byte("the body"))
	if err != nil {
		t.Fatal(err)
	}
	token := "sealed-v1:" + base64.StdEncoding.EncodeToString(sealed)

	got := toolbox.Open("before "+token+" after", me)
	if got != "before the body after" {
		t.Errorf("got %q", got)
	}

	other, _ := identity.Generate()
	if got := toolbox.Open(token, other); got != "[sealed: cannot open]" {
		t.Errorf("another key read the token: %q", got)
	}
}

func TestWritesPetnames(t *testing.T) {
	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	got := toolbox.Petnames("from " + me.EncodePublicKey())
	if got != "from "+me.Name() {
		t.Errorf("got %q, want the name %q", got, me.Name())
	}

	// A shorter run of hex is not a key, and stays as it is.
	if got := toolbox.Petnames("abc123"); got != "abc123" {
		t.Errorf("got %q", got)
	}
}

func TestPreviewCutsTheLastField(t *testing.T) {
	line := "m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\t" + strings.Repeat("x", 40)

	got := toolbox.Preview(thread(line), 10)
	fields := strings.Split(strings.Split(got, "\n")[1], "\t")

	if last := fields[len(fields)-1]; last != strings.Repeat("x", 9)+"…" {
		t.Errorf("last field = %q", last)
	}
	if len(fields) != 7 {
		t.Errorf("the record lost fields: %d", len(fields))
	}
}

func TestReadsARecordWithSeveralLines(t *testing.T) {
	text := thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\tfirst", "second", "third")

	records := toolbox.Records(text)
	if len(records) != 1 {
		t.Fatalf("read %d records, want 1", len(records))
	}
	if records[0].Body != "first\nsecond\nthird" {
		t.Errorf("body = %q", records[0].Body)
	}
}

func TestReadsFlags(t *testing.T) {
	text := thread("m1\tout\trose\t2026-01-01T10:00:00Z\tm0\tread;reaction=👍:ivy;attach=notes.txt:120\thello")

	records := toolbox.Records(text)
	if len(records) != 1 {
		t.Fatalf("read %d records", len(records))
	}

	record := records[0]
	if record.State != "read" || record.ReplyTo != "m0" || record.Direction != "out" {
		t.Errorf("record = %+v", record)
	}
	if len(record.Reactions) != 1 || record.Reactions[0] != [2]string{"👍", "ivy"} {
		t.Errorf("reactions = %v", record.Reactions)
	}
	if len(record.Attachments) != 1 || record.Attachments[0] != [2]string{"notes.txt", "120"} {
		t.Errorf("attachments = %v", record.Attachments)
	}
}

func TestShowsAConversation(t *testing.T) {
	text := thread(
		"aaaaaaaam1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\thello",
		"aaaaaaaam2\tout\trose\t2026-01-01T10:05:00Z\taaaaaaaam1\tdelivered\thi back")

	got := toolbox.Conversation(text)

	for _, want := range []string{"── thread with rose ──", "rose  2026-01-01 10:00", "  hello", "you  2026-01-01 10:05", "↳ reply to aaaam1", "✓ delivered"} {
		if !strings.Contains(got, want) {
			t.Errorf("the conversation is missing %q:\n%s", want, got)
		}
	}
}

func TestShowsARetractedMessage(t *testing.T) {
	text := thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tretracted\tthe old body")

	if got := toolbox.Conversation(text); !strings.Contains(got, "(retracted)") || strings.Contains(got, "the old body") {
		t.Errorf("got:\n%s", got)
	}
	if got := toolbox.Markdown(text); !strings.Contains(got, "_(retracted)_") || strings.Contains(got, "the old body") {
		t.Errorf("got:\n%s", got)
	}
}

func TestWritesMarkdown(t *testing.T) {
	text := thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\thello")

	got := toolbox.Markdown(text)
	for _, want := range []string{"# thread with rose", "## rose — 2026-01-01T10:00:00Z", "<!-- id: m1 -->", "hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("the document is missing %q:\n%s", want, got)
		}
	}
}

func TestReadsTheFiltersOfACommand(t *testing.T) {
	spec := map[string]any{"output": map[string]any{
		"filters": []any{"open", "petnames", 7, "preview:20"},
	}}

	got := toolbox.OutputFilters(spec)
	want := []string{"open", "petnames", "preview:20"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("filters = %v, want %v", got, want)
	}
	if got := toolbox.OutputFilters(map[string]any{}); got != nil {
		t.Errorf("a command without output gave %v", got)
	}
}

func TestRunsTheFiltersInOrder(t *testing.T) {
	me, _ := identity.Generate()
	sealed, _ := sealedbox.SealTo(me.PublicKey, []byte("opened"))
	token := "sealed-v1:" + base64.StdEncoding.EncodeToString(sealed)

	seen := ""
	extra := map[string]func(string) string{
		"cache": func(text string) string { seen = text; return text },
	}

	got := toolbox.ApplyOutput(
		thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\t"+token),
		[]string{"open", "cache"}, me, extra)

	if !strings.Contains(got, "opened") {
		t.Errorf("the token did not open: %q", got)
	}
	if !strings.Contains(seen, "opened") {
		t.Error("the cache ran before the token opened")
	}
}

func TestAnUnknownFilterChangesNothing(t *testing.T) {
	text := thread("m1\tin\trose\t2026-01-01T10:00:00Z\t-\tread\thello")

	if got := toolbox.ApplyOutput(text, []string{"nothing-of-that-name"}, nil, nil); got != text {
		t.Errorf("got %q", got)
	}
}
