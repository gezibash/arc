package journal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/journal"
)

// machine is one node of the owner, with its own store.
type machine struct {
	node    *node.Node
	journal *journal.Journal
}

func newMachine(t *testing.T, k keys.Key, transports ...transport.Transport) machine {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	n := &node.Node{Store: s}
	j, err := journal.New(k, n, transports)
	if err != nil {
		t.Fatal(err)
	}
	return machine{node: n, journal: j}
}

func read(t *testing.T, m machine, address string, lines journal.Lines) string {
	t.Helper()
	var out bytes.Buffer
	if _, err := m.journal.Read(context.Background(), address, lines, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// numbered makes text of n lines, each long enough to fill parts quickly.
func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %05d %s\n", i, strings.Repeat("x", 90))
	}
	return b.String()
}

func TestWriteAndRead(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t, keys.Generate())

	page, err := m.journal.Write(ctx, "hrs/ablations/lr-sweep", "LR sweep", strings.NewReader("auc 0.871\nnext: warmup\n"))
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "LR sweep" || len(page.Parts) != 1 {
		t.Errorf("page = %+v", page)
	}
	if got := read(t, m, "hrs/ablations/lr-sweep", journal.Lines{}); got != "auc 0.871\nnext: warmup\n" {
		t.Errorf("read %q", got)
	}
}

func TestAStoreHoldsNoAddressAndNoText(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t, keys.Generate())

	if _, err := m.journal.Write(ctx, "secret-project/notes/plan", "The plan", strings.NewReader("the launch is on friday\n")); err != nil {
		t.Fatal(err)
	}

	for _, event := range m.node.Store.Query(m.journal.Filter()) {
		body, _ := json.Marshal(event)
		for _, word := range []string{"secret-project", "The plan", "friday"} {
			if bytes.Contains(body, []byte(word)) {
				t.Errorf("an event holds %q in the clear: %s", word, body)
			}
		}
	}
}

func TestAnotherKeyCannotReadThePage(t *testing.T) {
	ctx := context.Background()
	url := testrelay.Start(t)
	r := relay.Relay{URL: url}

	owner := newMachine(t, keys.Generate(), r)
	if _, err := owner.journal.Write(ctx, "a/b/c", "", strings.NewReader("mine\n")); err != nil {
		t.Fatal(err)
	}

	stranger := newMachine(t, keys.Generate(), r)
	if _, err := stranger.journal.Page(ctx, "a/b/c"); err != journal.ErrNotFound {
		t.Errorf("a stranger found the page: %v", err)
	}
}

func TestALargePageTravelsAsParts(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t, keys.Generate())
	text := numbered(2000) // about 200 KB

	page, err := m.journal.Write(ctx, "big/page/one", "", strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Parts) < 6 {
		t.Errorf("the page has %d parts, want at least 6", len(page.Parts))
	}
	if got := read(t, m, "big/page/one", journal.Lines{}); got != text {
		t.Errorf("the page came back with %d bytes, want %d", len(got), len(text))
	}

	// Every event stays under the 64 KiB limit of common relays.
	for _, event := range m.node.Store.Query(m.journal.Filter()) {
		body, _ := json.Marshal(event)
		if len(body) >= 64*1024 {
			t.Errorf("an event of kind %d is %d bytes", event.Kind, len(body))
		}
	}
}

func TestARangeReadFetchesOnlyItsParts(t *testing.T) {
	ctx := context.Background()
	k := keys.Generate()
	r := relay.Relay{URL: testrelay.Start(t)}

	writer := newMachine(t, k, r)
	page, err := writer.journal.Write(ctx, "big/page/two", "", strings.NewReader(numbered(2000)))
	if err != nil {
		t.Fatal(err)
	}

	// A second machine holds nothing, and reads lines 1000 to 1002.
	reader := newMachine(t, k, r)
	got := read(t, reader, "big/page/two", journal.Lines{From: 1000, To: 1002})

	want := strings.Join(strings.Split(numbered(2000), "\n")[999:1002], "\n") + "\n"
	if got != want {
		t.Errorf("read %q, want %q", got, want)
	}

	parts := reader.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{journal.PartKind}})
	if len(parts) == 0 || len(parts) > 2 {
		t.Errorf("the reader fetched %d of %d parts for three lines", len(parts), len(page.Parts))
	}
}

func TestAppendJoinsTheLastPart(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t, keys.Generate())
	address := "hrs/log/today"

	for i := 1; i <= 5; i++ {
		if _, err := m.journal.Append(ctx, address, strings.NewReader(fmt.Sprintf("note %d\n", i))); err != nil {
			t.Fatal(err)
		}
	}

	page, err := m.journal.Page(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Parts) != 1 {
		t.Errorf("five small notes made %d parts, want 1", len(page.Parts))
	}
	if got := read(t, m, address, journal.Lines{}); got != "note 1\nnote 2\nnote 3\nnote 4\nnote 5\n" {
		t.Errorf("read %q", got)
	}
}

func TestAppendPastAPartStartsANewOne(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t, keys.Generate())
	address := "hrs/log/long"
	first := numbered(300) // about 30 KB

	if _, err := m.journal.Write(ctx, address, "", strings.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	more := numbered(50)
	page, err := m.journal.Append(ctx, address, strings.NewReader(more))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Parts) != 2 {
		t.Errorf("the page has %d parts, want 2", len(page.Parts))
	}
	if got := read(t, m, address, journal.Lines{}); got != first+more {
		t.Error("the appended page does not read back whole")
	}
}

func TestTwoWritesInOneSecondKeepTheSecond(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t, keys.Generate())

	for _, text := range []string{"first\n", "second\n"} {
		if _, err := m.journal.Write(ctx, "a/b/c", "", strings.NewReader(text)); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(t, m, "a/b/c", journal.Lines{}); got != "second\n" {
		t.Errorf("read %q, want the second write", got)
	}
}

func TestRefusesAnAddressThatIsNotThreeParts(t *testing.T) {
	m := newMachine(t, keys.Generate())
	for _, address := range []string{"a/b", "a/b/c/d", "A/b/c", "a/../c", "a/b/-c"} {
		if _, err := m.journal.Write(context.Background(), address, "", strings.NewReader("x")); err != journal.ErrAddress {
			t.Errorf("%q: error = %v, want ErrAddress", address, err)
		}
	}
}

func TestParseLines(t *testing.T) {
	good := map[string]journal.Lines{"": {}, "3:5": {From: 3, To: 5}, "7:": {From: 7}, ":2": {To: 2}}
	for in, want := range good {
		got, err := journal.ParseLines(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v, %v", in, got, err)
		}
	}
	for _, in := range []string{"5", "0:2", "5:3", "a:b"} {
		if _, err := journal.ParseLines(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t, keys.Generate())
	for _, address := range []string{"b/x/y", "a/x/y"} {
		if _, err := m.journal.Write(ctx, address, "T "+address, strings.NewReader("x\n")); err != nil {
			t.Fatal(err)
		}
	}

	pages, unreadable := m.journal.List()
	if unreadable != 0 || len(pages) != 2 || pages[0].Address != "a/x/y" || pages[1].Title != "T b/x/y" {
		t.Errorf("list = %+v, unreadable %d", pages, unreadable)
	}
}

// syncBuffer is a writer that a test can read while another goroutine writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out.String() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tail wrote %q, want %q", out.String(), want)
}

func TestTailStreamsWhatIsAppended(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k := keys.Generate()
	r := relay.Relay{URL: testrelay.Start(t)}
	address := "hrs/log/live"

	writer := newMachine(t, k, r)
	if _, err := writer.journal.Write(ctx, address, "", strings.NewReader("start\n")); err != nil {
		t.Fatal(err)
	}

	watcher := newMachine(t, k, r)
	out := &syncBuffer{}
	go func() { _ = watcher.journal.Tail(ctx, address, r, out) }()
	waitFor(t, out, "start\n")

	for _, note := range []string{"one\n", "two\n"} {
		if _, err := writer.journal.Append(ctx, address, strings.NewReader(note)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, out, "start\none\ntwo\n")

	if _, err := writer.journal.Write(ctx, address, "", strings.NewReader("fresh\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "start\none\ntwo\n\n--- "+address+" was rewritten ---\nfresh\n")
}
