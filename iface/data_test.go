package iface

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"fiatjaf.com/nostr/nip70"
	"github.com/gezibash/arc/delivery/draft"
	"github.com/gezibash/arc/delivery/store"
)

// citizen is one machine with a store, and the journal and files installed.
type citizen struct {
	t      *testing.T
	env    *fakeEnv
	author nostr.PubKey
}

func newCitizen(t *testing.T) *citizen {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return &citizen{t: t, env: &fakeEnv{t: t, net: newNet(), me: nostr.Generate(), store: s}, author: nostr.Generate().Public()}
}

func (c *citizen) run(id, stdin string, words ...string) (string, error) {
	c.t.Helper()
	m := specManifests(c.t)[id]
	var out, errs bytes.Buffer
	err := Run(context.Background(), c.env, Installed{Manifest: m, Author: c.author, Name: id}, words,
		Stdio{In: strings.NewReader(stdin), Out: &out, Err: &errs})
	return out.String(), err
}

func (c *citizen) must(id, stdin string, words ...string) string {
	c.t.Helper()
	out, err := c.run(id, stdin, words...)
	if err != nil {
		c.t.Fatalf("%s %v: %v", id, words, err)
	}
	return out
}

const page = "hrs/ablations/lr-sweep"

func TestAJournalPageIsWrittenReadAndAppended(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "auc 0.871\n", "write", page, "--title", "LR sweep")
	if got := c.must("journal", "", "read", page); got != "auc 0.871\n" {
		t.Fatalf("read %q", got)
	}
	c.must("journal", "", "append", page, "next:", "try", "warmup")
	if got := c.must("journal", "", "read", page); got != "auc 0.871\nnext: try warmup\n" {
		t.Errorf("after append, read %q", got)
	}
	if got := c.must("journal", "", "ls"); !strings.Contains(got, page+"\tLR sweep\t") {
		t.Errorf("append lost the title: %q", got)
	}
	if got := c.must("journal", "", "read", page, "--lines", "2:"); got != "next: try warmup\n" {
		t.Errorf("lines 2: read %q", got)
	}
}

func TestThePageIsANIP37DraftOfAnArticle(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "short page\n", "write", page, "--title", "T")
	wraps := c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}})
	if len(wraps) != 1 {
		t.Fatalf("the store holds %d drafts", len(wraps))
	}
	if strings.Contains(wraps[0].Tags.GetD(), "hrs") || strings.Contains(wraps[0].Content, "short page") {
		t.Error("the draft shows the address or the text")
	}
	opened, err := draft.Open(context.Background(), c.env.Keyer(), wraps[0])
	if err != nil || opened.Event.Kind != 30023 || opened.Event.Tags.GetD() != page || opened.Event.Content != "short page\n" || opened.Parts() != nil {
		t.Errorf("the draft holds %+v %v", opened.Event, err)
	}
}

func TestALongPageTravelsInParts(t *testing.T) {
	c := newCitizen(t)
	var body strings.Builder
	for i := 0; body.Len() < 3*draft.PartSize; i++ {
		body.WriteString(strings.Repeat("x", 70) + " line " + time.Duration(i).String() + "\n")
	}
	c.must("journal", body.String(), "write", page)
	if got := c.must("journal", "", "read", page); got != body.String() {
		t.Fatalf("the long page reads back with %d bytes, want %d", len(got), body.Len())
	}
	if n := len(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.PartKind}})); n != 3 {
		t.Errorf("the page has %d parts, want 3", n)
	}

	// Append changes only the last part.
	c.must("journal", "", "append", page, "the", "end")
	if got := c.must("journal", "", "read", page); got != body.String()+"the end\n" {
		t.Errorf("after append the page ends %q", got[len(got)-20:])
	}
	if n := len(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.PartKind}})); n != 4 {
		t.Errorf("append made %d parts in all, want 4: three, and one new last part", n)
	}
}

func TestAMissingPartKeepsTheTextBeforeIt(t *testing.T) {
	c := newCitizen(t)
	body := strings.Repeat(strings.Repeat("y", 99)+"\n", 800) // 80,000 bytes
	c.must("journal", body, "write", page)
	parts := c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.PartKind}})
	// Take away the last part, as a transport that never brought it.
	wraps := c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}})
	opened, _ := draft.Open(context.Background(), c.env.Keyer(), wraps[0])
	last := opened.Parts()[len(opened.Parts())-1]
	for _, p := range parts {
		if p.ID.Hex() == last {
			request := nostr.Event{Kind: 5, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"e", last}}}
			request.Sign(c.env.me)
			c.env.store.Save(request)
		}
	}
	out, err := c.run("journal", "", "read", page)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("got %v", err)
	}
	if !strings.HasPrefix(body, out) || len(out) < draft.PartSize-100 {
		t.Errorf("the text before the missing part is %d bytes", len(out))
	}
}

func TestListSearchHistoryAndDelete(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "warmup helps\n", "write", "hrs/ablations/warmup", "--title", "Warmup")
	c.must("journal", "the learning rate sweep\n", "write", page, "--title", "LR sweep")
	c.must("journal", "groceries\n", "write", "home/list/shop")
	c.must("journal", "", "append", page, "second", "revision")

	if got := c.must("journal", "", "ls", "hrs/"); !strings.HasPrefix(got, "hrs/ablations/lr-sweep\t") || strings.Contains(got, "home/") || strings.Count(got, "\n") != 2 {
		t.Errorf("ls hrs/ shows %q", got)
	}
	if got := c.must("journal", "", "search", "warmup"); !strings.HasPrefix(got, "hrs/ablations/warmup\t") || strings.Count(got, "\n") != 1 {
		t.Errorf("search shows %q", got)
	}
	history := c.must("journal", "", "history", page)
	if strings.Count(history, "\n  the learning rate sweep\n") != 2 || !strings.HasSuffix(history, "  second revision\n") {
		t.Errorf("history shows %q", history)
	}

	c.must("journal", "", "delete", page)
	if got := c.must("journal", "", "read", page); got != "" {
		t.Errorf("a deleted page reads %q", got)
	}
	if got := c.must("journal", "", "ls", "hrs/"); strings.Contains(got, "lr-sweep") {
		t.Errorf("ls still shows the deleted page: %q", got)
	}
	if got := c.must("journal", "", "history", page); got != "no revisions\n" {
		t.Errorf("the history of a deleted page is %q", got)
	}
	if _, err := c.run("journal", "", "delete", page); err == nil {
		t.Error("a second delete found something to delete")
	}
	// A new page at the same address starts again.
	c.must("journal", "fresh\n", "write", page)
	if got := c.must("journal", "", "read", page); got != "fresh\n" {
		t.Errorf("a new page after delete reads %q", got)
	}
}

func TestKPIs(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "", "kpi", "set", "hrs", "auc", "0.85")
	c.must("journal", "", "kpi", "set", "hrs", "auc", "0.87", "--note", "warmup")
	c.must("journal", "", "kpi", "set", "hrs", "loss", "0.3")
	c.must("journal", "", "kpi", "set", "home", "steps", "9000")

	latest := c.must("journal", "", "kpi", "latest", "hrs")
	if !strings.Contains(latest, "\tauc\t0.87\twarmup") || !strings.Contains(latest, "\tloss\t0.3\t") || strings.Contains(latest, "0.85") || strings.Contains(latest, "steps") {
		t.Errorf("latest shows %q", latest)
	}
	log := c.must("journal", "", "kpi", "log", "hrs", "auc")
	if lines := strings.Split(strings.TrimSpace(log), "\n"); len(lines) != 2 || !strings.Contains(lines[0], "0.85") {
		t.Errorf("log shows %q", log)
	}
}

func TestFilesGoInAndComeOut(t *testing.T) {
	c := newCitizen(t)
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0, 1, 2, 250, 'a', '\n'}, 20000) // 120,000 bytes, binary
	in := filepath.Join(dir, "weights.bin")
	os.WriteFile(in, data, 0o600)
	sum := sha256.Sum256(data)

	id := strings.TrimSpace(c.must("files", "", "put", in))
	if id != hex.EncodeToString(sum[:]) {
		t.Fatalf("put printed %q", id)
	}
	if got := c.must("files", "", "list"); !strings.Contains(got, "weights.bin\t120000 bytes") {
		t.Errorf("list shows %q", got)
	}

	out := filepath.Join(dir, "back.bin")
	c.must("files", "", "get", id, "--output", out)
	back, _ := os.ReadFile(out)
	if !bytes.Equal(back, data) {
		t.Errorf("the file came back with %d bytes", len(back))
	}
	if _, err := c.run("files", "", "get", id, "--output", out); err == nil || !strings.Contains(err.Error(), "never replaces") {
		t.Errorf("get replaced a file: %v", err)
	}
}

func TestTailShowsOnlyWhatIsAppended(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "first\n", "write", page)

	c.env.live = make(chan nostr.Event, 64)
	done := make(chan string)
	go func() {
		var out bytes.Buffer
		m := specManifests(t)["journal"]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		Run(ctx, c.env, Installed{Manifest: m, Author: c.author, Name: "journal"}, []string{"tail", page},
			Stdio{In: strings.NewReader(""), Out: &out, Err: &bytes.Buffer{}})
		done <- out.String()
	}()
	time.Sleep(100 * time.Millisecond)
	c.must("journal", "", "append", page, "second")
	c.must("journal", "rewritten\n", "write", page)
	time.Sleep(100 * time.Millisecond)
	close(c.env.live)

	got := <-done
	want := "first\nsecond\n--- rewritten ---\nrewritten\n"
	if got != want {
		t.Errorf("tail showed %q, want %q", got, want)
	}
}

func TestSealedDataBelongsToItsAuthor(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", "mine\n", "write", page)
	other := newCitizen(t)
	other.env.store = c.env.store // the same store, as a relay that holds both
	other.author = c.author
	if got := other.must("journal", "", "ls"); strings.Contains(got, page) {
		t.Errorf("another citizen lists the page: %q", got)
	}
}

func TestARangeFetchesOnlyItsParts(t *testing.T) {
	c := newCitizen(t)
	var body strings.Builder
	for i := 1; i <= 3000; i++ {
		body.WriteString(strings.Repeat("z", 90) + " " + time.Duration(i).String() + "\n")
	}
	c.must("journal", body.String(), "write", page)
	lines := strings.SplitAfter(body.String(), "\n")

	c.env.fetchedIDs = 0
	got := c.must("journal", "", "read", page, "--lines", "1500:1501")
	if got != lines[1499]+lines[1500] {
		t.Errorf("lines 1500:1501 read %q", got)
	}
	if c.env.fetchedIDs > 2 {
		t.Errorf("a range of two lines fetched %d parts", c.env.fetchedIDs)
	}
	if got := c.must("journal", "", "read", page, "--lines", "2999:"); got != lines[2998]+lines[2999] {
		t.Errorf("lines 2999: read %q", got)
	}
	if got := c.must("journal", "", "read", page, "--lines", ":2"); got != lines[0]+lines[1] {
		t.Errorf("lines :2 read %q", got)
	}

	// After an append, the counts still hold.
	c.must("journal", "", "append", page, "last", "line")
	if got := c.must("journal", "", "read", page, "--lines", "3001:"); got != "last line\n" {
		t.Errorf("after append, line 3001 reads %q", got)
	}
}

// pair makes two citizens on one network, with one author for manifests.
func pair(t *testing.T) (*citizen, *citizen) {
	a, b := newCitizen(t), newCitizen(t)
	b.env.net = a.env.net
	b.author = a.author
	return a, b
}

func TestDirectMessages(t *testing.T) {
	alice, bob := pair(t)
	bobKey := nip19.EncodeNpub(bob.env.me.Public())
	alice.must("dm", "", "send", bobKey, "meet", "at", "noon")
	bob.must("dm", "", "send", alice.env.me.Public().Hex(), "see you there")

	inbox := bob.must("dm", "", "inbox")
	if !strings.Contains(inbox, "meet at noon") || strings.Contains(inbox, "see you there") {
		t.Errorf("bob's inbox is %q", inbox)
	}
	conversation := alice.must("dm", "", "open", bobKey)
	first, second := strings.Index(conversation, "meet at noon"), strings.Index(conversation, "see you there")
	if first < 0 || second < 0 {
		t.Errorf("the conversation is %q", conversation)
	}
	rumors := alice.env.net.inbox[bob.env.me.Public()]
	if rumors[0].Kind != 14 || rumors[0].Tags.Find("p")[1] != bob.env.me.Public().Hex() {
		t.Errorf("the message is %+v", rumors[0])
	}
}

func TestAgoraPostsRepliesAndThreads(t *testing.T) {
	alice, bob := pair(t)
	alice.must("agora", "", "post", "--title", "Hello", "first", "post")
	board := alice.env.net.relays["wss://board.example"]
	posts := board.Query(nostr.Filter{Kinds: []nostr.Kind{11}})
	if len(posts) != 1 {
		t.Fatalf("the board holds %d posts", len(posts))
	}
	post := posts[0]
	if h := post.Tags.Find("h"); h == nil || h[1] != "agora" || !nip70.IsProtected(post) {
		t.Errorf("the post lacks its group tags: %v", post.Tags)
	}

	feed := bob.must("agora", "", "feed")
	if !strings.Contains(feed, "Hello") || !strings.Contains(feed, "first post") {
		t.Errorf("the feed is %q", feed)
	}

	root := nip19.EncodeNevent(post.ID, nil, nostr.ZeroPK)
	bob.must("agora", "", "reply", root, "welcome")
	time.Sleep(1100 * time.Millisecond)
	alice.must("agora", "", "reply", root, "thanks")
	replies := board.Query(nostr.Filter{Kinds: []nostr.Kind{1111}})
	if len(replies) != 2 {
		t.Fatalf("the board holds %d replies", len(replies))
	}
	for _, reply := range replies {
		for _, name := range []string{"E", "e"} {
			if tag := reply.Tags.Find(name); tag == nil || tag[1] != post.ID.Hex() || tag[3] != alice.env.me.Public().Hex() {
				t.Errorf("the %s tag is %v", name, tag)
			}
		}
		for _, name := range []string{"P", "p"} {
			if tag := reply.Tags.Find(name); tag == nil || tag[1] != alice.env.me.Public().Hex() {
				t.Errorf("the %s tag is %v", name, tag)
			}
		}
		if k := reply.Tags.Find("K"); k == nil || k[1] != "11" {
			t.Errorf("the K tag is %v", k)
		}
	}

	thread := bob.must("agora", "", "thread", root)
	if w, th := strings.Index(thread, "welcome"), strings.Index(thread, "thanks"); w < 0 || th < w {
		t.Errorf("the thread is %q", thread)
	}

	alice.must("agora", "", "remove", root)
	removals := board.Query(nostr.Filter{Kinds: []nostr.Kind{9005}})
	if len(removals) != 1 || removals[0].Tags.Find("e")[1] != post.ID.Hex() || removals[0].Tags.Find("h")[1] != "agora" {
		t.Errorf("the removal is %+v", removals)
	}
}

func TestThreadOrdersReplies(t *testing.T) {
	e := func(id, parent string, at int64) *entry {
		return &entry{rec: Record{"id": id, "created": at, "tags": map[string]any{"e": parent}}}
	}
	got := thread([]*entry{e("c", "a", 3), e("b", "root", 2), e("a", "root", 1), e("d", "b", 4)}, "tags.e")
	var order []string
	for _, x := range got {
		order = append(order, str(x.rec["id"])+str(x.rec["depth"]))
	}
	if strings.Join(order, " ") != "a0 c1 b0 d1" {
		t.Errorf("the thread is %v", order)
	}
}

func TestADryRunSignsNothing(t *testing.T) {
	alice, bob := pair(t)
	out := alice.must("journal", "secret text\n", "write", page, "--dry-run")
	if !strings.Contains(out, `"kind": 30023`) || !strings.Contains(out, "secret text") {
		t.Errorf("the dry run shows %q", out)
	}
	if n := len(alice.env.store.Query(nostr.Filter{})); n != 0 {
		t.Errorf("a dry run stored %d events", n)
	}
	alice.must("dm", "", "send", "--dry-run", bob.env.me.Public().Hex(), "hi")
	alice.must("agora", "", "post", "--dry-run", "hello")
	if len(alice.env.net.inbox) != 0 || len(alice.env.net.relays) != 0 {
		t.Errorf("a dry run sent something: %d inboxes, relays %v", len(alice.env.net.inbox), alice.env.net.relays)
	}
	alice.must("journal", "real\n", "write", page)
	alice.must("journal", "", "delete", page, "--dry-run")
	if got := alice.must("journal", "", "read", page); got != "real\n" {
		t.Errorf("a dry run of delete deleted: %q", got)
	}
}

func TestReservedKindsAndConsent(t *testing.T) {
	base := `{"interface": 1, "id": "x", "shape": "data", "title": "X", "formats": {},
	 "kinds": {"k": {"kind": KIND, "visibility": "VIS"}},
	 "commands": [{"path": ["q"], "summary": "s", "action": {"query": {"kinds": ["k"]}}}]}`
	make := func(kind, vis string) string {
		return strings.NewReplacer("KIND", kind, "VIS", vis).Replace(base)
	}
	for _, c := range []struct{ kind, vis, want string }{
		{"0", "public", "reserved"}, {"3", "public", "reserved"}, {"31234", "sealed", "reserved"},
		{"24133", "public", "reserved"}, {"39000", "public", "group state"}, {"9005", "public", "must be group"},
	} {
		if _, err := Parse([]byte(make(c.kind, c.vis))); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("kind %s %s: got %v", c.kind, c.vis, err)
		}
	}

	old, _ := Parse([]byte(make("30023", "sealed")))
	consent := ConsentOf(old)
	if got := consent.Changes(old); len(got) != 0 {
		t.Errorf("the same manifest needs consent again: %v", got)
	}
	louder, _ := Parse([]byte(make("30023", "public")))
	if got := consent.Changes(louder); len(got) != 1 || !strings.Contains(got[0], "public now") {
		t.Errorf("a louder kind: %v", got)
	}
	other, _ := Parse([]byte(make("30024", "sealed")))
	if got := consent.Changes(other); len(got) != 1 || !strings.Contains(got[0], "30024") {
		t.Errorf("a new kind: %v", got)
	}
	quieter := ConsentOf(louder)
	if got := quieter.Changes(old); len(got) != 0 {
		t.Errorf("a quieter kind needs consent: %v", got)
	}
	if d := Describe(louder); !strings.Contains(d, "posted in public") || !strings.Contains(d, "post under your name") {
		t.Errorf("describe: %s", d)
	}
}
