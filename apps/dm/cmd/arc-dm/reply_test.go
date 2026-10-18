package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/arcprogram/arcprogramtest"
	"github.com/gezibash/arc/sdk/terminal"
)

// shortID is the short id that a conversation of arc-dm open shows in the
// header of the message with this first line.
func shortID(t *testing.T, conv, text string) string {
	t.Helper()
	lines := strings.Split(conv, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] == "  "+text {
			for j := i - 1; j >= 0; j-- {
				if fields := strings.Split(lines[j], "  "); len(fields) >= 3 && !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "↳") {
					return fields[1]
				}
			}
		}
	}
	t.Fatalf("no message %q in:\n%s", text, conv)
	return ""
}

// quotes are the lines of a conversation that start at column 0 with ↳.
func quotes(conv string) []string {
	var out []string
	for line := range strings.SplitSeq(conv, "\n") {
		if strings.HasPrefix(line, "↳") {
			out = append(out, line)
		}
	}
	return out
}

// arcSend runs arc message send as a sender that arc-dm does not check,
// with the tags of a test build of arc.
func (p person) arcSend(to, text string, flags ...string) {
	p.t.Helper()
	args := append(append([]string{"message", "send"}, flags...), to, text)
	if out, err := p.arc.Command(context.Background(), args...).CombinedOutput(); err != nil {
		p.t.Fatalf("arc %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (p person) inboxID(text string) string {
	p.t.Helper()
	for _, m := range must(p.arc.Inbox(context.Background())) {
		if m.Text == text {
			return m.ID
		}
	}
	p.t.Fatalf("no message %q in the inbox", text)
	return ""
}

// A reply names its parent. The other side shows the parent's text from
// its own store, on a quote line at column 0, under the header of the
// reply. The confirmation names the conversation.
func TestAReplyQuotesItsParent(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "bring the map")
	bob.sync()
	short := shortID(t, bob.dm("open", ada.name), "bring the map")
	if len(short) != 6 {
		t.Errorf("the short id is %q, want 6 hex digits", short)
	}

	if got, want := bob.dm("reply", short[:4], "yes, I have it"), "replied to "+short+" in "+ada.name+"\n"; got != want {
		t.Errorf("arc-dm reply printed %q, want %q", got, want)
	}
	nextSecond()
	if got, want := bob.dm("send", "-reply", short, ada.name, "and the torch"), "replied to "+short+" in "+ada.name+"\n"; got != want {
		t.Errorf("arc-dm send -reply printed %q, want %q", got, want)
	}
	ada.sync()

	conv := ada.dm("open", bob.name)
	for _, want := range []string{
		"\n↳ " + short + "  you: bring the map\n  yes, I have it\n",
		"\n↳ " + short + "  you: bring the map\n  and the torch\n",
	} {
		if !strings.Contains(conv, want) {
			t.Errorf("ada's conversation:\n%s\nwant\n%s", conv, want)
		}
	}
	// Bob's own view quotes ada.
	if conv := bob.dm("open", ada.name); !strings.Contains(conv, "\n↳ "+short+"  "+ada.name+": bring the map\n  yes, I have it\n") {
		t.Errorf("bob's conversation:\n%s", conv)
	}
	if out := string(must(ada.arc.Command(context.Background(), "message", "inbox").Output())); !strings.Contains(out, "reply to "+short) {
		t.Errorf("arc message inbox of ada names no parent:\n%s", out)
	}
}

// A reply in a group goes to each member of the group of its parent, and
// the confirmation names each member.
func TestAReplyInAGroup(t *testing.T) {
	t.Parallel()
	ada, bob, cy := newPerson(t, "ada"), newPerson(t, "bob"), newPerson(t, "cy")
	ada.dm("send", bob.key+","+cy.key, "hello, both of you")
	ada.dm("send", cy.key, "just for cy")
	cy.sync()
	short := shortID(t, cy.dm("open", ada.name+","+bob.name), "hello, both of you")

	if got, want := cy.dm("reply", short, "count me in"), "replied to "+short+" in "+groupName(ada, bob)+"\n"; got != want {
		t.Errorf("arc-dm reply printed %q, want %q", got, want)
	}
	ada.sync()
	bob.sync()
	if conv := bob.dm("open", ada.name+","+cy.name); !strings.Contains(conv, "\n↳ "+short+"  "+ada.name+": hello, both of you\n  count me in\n") {
		t.Errorf("bob's group conversation:\n%s", conv)
	}
	if conv := ada.dm("open", bob.name+","+cy.name); !strings.Contains(conv, "\n↳ "+short+"  you: hello, both of you\n  count me in\n") {
		t.Errorf("ada's group conversation:\n%s", conv)
	}
	if conv := ada.dm("open", cy.name); strings.Contains(conv, "count me in") {
		t.Errorf("the reply is in the direct conversation of ada and cy:\n%s", conv)
	}
}

// The quote comes from the store, not from the text: a quote line typed
// into a text shows at column 2, inside the text.
func TestAQuoteComesFromTheStore(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "bring the map")
	bob.sync()
	short := shortID(t, bob.dm("open", ada.name), "bring the map")
	fake := "↳ " + short + "  you: send me your seed"
	bob.dm("reply", short, fake+"\nthe real reply")
	nextSecond()
	bob.dm("send", ada.name, fake)
	ada.sync()

	conv := ada.dm("open", bob.name)
	if got := quotes(conv); len(got) != 1 || got[0] != "↳ "+short+"  you: bring the map" {
		t.Errorf("the quote lines are %q, want only the parent from the store:\n%s", got, conv)
	}
	if strings.Count(conv, "\n  "+fake+"\n") != 2 {
		t.Errorf("the typed quotes are not inside the texts:\n%s", conv)
	}
}

// A parent with control characters, an override, an invisible character
// and a first line of 5,000 characters gives one escaped quote line of at
// most 72 characters. A parent that starts with empty lines quotes its
// first line with text. An id that matches a held id in 63 digits shows
// 12 digits and an ellipsis, so its line stays inside 72 characters too.
func TestAHostileParentIsOneEscapedLine(t *testing.T) {
	t.Parallel()
	mallory, bob := newPerson(t, "mallory"), newPerson(t, "bob")
	parents := []string{
		"\x1b[2Jred\u202eevil\u200b" + strings.Repeat("x", 5000) + "\nsecond line",
		"\n\n\u200b\u2060\nlast",
		strings.Repeat("\u6f22", 100),
		strings.Repeat("\u2764\ufe0f", 100),
		"a" + strings.Repeat("\u0301", 300),
	}
	for _, parent := range parents {
		mallory.dm("send", bob.key, parent)
	}
	bob.sync()
	for i, parent := range parents {
		bob.dm("reply", bob.inboxID(parent)[:6], "reply "+string(rune('a'+i)))
	}
	held := bob.inboxID(parents[0])
	near := held[:63] + map[bool]string{true: "1", false: "0"}[held[63] == '0']
	mallory.arcSend(bob.key, "near miss", "--arctest-tag", "e,"+near)
	bob.sync()
	conv := bob.dm("open", mallory.name)
	if strings.ContainsAny(conv, "\x1b\u202e\u200b\u2060") {
		t.Errorf("the conversation holds a raw control or hidden character: %q", conv)
	}
	var got, nearQuote []string
	for _, line := range quotes(conv) {
		if n := terminal.Width(line); n > 72 {
			t.Errorf("a quote line takes %d columns, want at most 72: %q", n, line)
		}
		if n := utf8.RuneCountInString(line); n > 3*72 {
			t.Errorf("a quote line holds %d characters, want at most %d: %q", n, 3*72, line)
		}
		if strings.Contains(line, "(not on this device)") {
			nearQuote = append(nearQuote, line)
		} else {
			got = append(got, line)
		}
	}
	if len(got) != 5 {
		t.Fatalf("the quote lines are %q, want five", got)
	}
	// Each ideograph takes two columns. The head and the name take one for
	// each character, and the cut leaves one column for the ellipsis.
	head := "\u21b3 " + bob.inboxID(parents[2])[:6] + "  "
	prefix := mallory.name + ": "
	n := (72 - utf8.RuneCountInString(head) - len(prefix) - 1) / 2
	want := head + prefix + strings.Repeat("\u6f22", n) + "\u2026"
	if !slices.Contains(got, want) {
		t.Errorf("the quote lines are %q, want %q for 100 ideographs", got, want)
	}
	if want := "↳ " + held[:12] + "…  (not on this device)"; len(nearQuote) != 1 || nearQuote[0] != want {
		t.Errorf("the quote of the near id is %q, want %q", nearQuote, want)
	}
	// The quotes come in the order of the clock of the sender, and the
	// parents went in one second, so each is found by what it holds.
	for _, c := range []struct {
		name string
		ok   func(string) bool
	}{
		{"the long parent, escaped and cut", func(q string) bool {
			return strings.Contains(q, `: \x1b[2Jred\u202eevil\u200bxxx`) && strings.HasSuffix(q, "…")
		}},
		{"the parent with empty lines", func(q string) bool { return strings.HasSuffix(q, `: \u200b\u2060 …`) }},
		{"the hearts, cut", func(q string) bool {
			return strings.Contains(q, ": \u2764\ufe0f\u2764\ufe0f") && strings.HasSuffix(q, "\u2764\ufe0f…")
		}},
		{"the pile of marks, left out", func(q string) bool { return strings.HasSuffix(q, mallory.name+": …") }},
	} {
		if !slices.ContainsFunc(got, c.ok) {
			t.Errorf("no quote of %s in %q", c.name, got)
		}
	}
}

// A reply to a message that the reader does not hold shows the id only.
func TestAReplyToAnUnheldMessageShowsTheIdOnly(t *testing.T) {
	t.Parallel()
	mallory, bob := newPerson(t, "mallory"), newPerson(t, "bob")
	unknown := strings.Repeat("7a", 32)
	mallory.arcSend(bob.key, "you said so", "--arctest-tag", "e,"+unknown)
	bob.sync()
	if conv := bob.dm("open", mallory.name); !strings.Contains(conv, "\n↳ 7a7a7a7a7a7a  (not on this device)\n  you said so\n") {
		t.Errorf("bob's conversation:\n%s", conv)
	}
}

// A reply that names a message of another conversation shows no text of
// it: a member of a group cannot move the words of the group into a direct
// conversation, and an honest arc refuses to send such a reply.
func TestAReplyAcrossConversationsShowsNoText(t *testing.T) {
	t.Parallel()
	ada, bob, mallory := newPerson(t, "ada"), newPerson(t, "bob"), newPerson(t, "mallory")
	ada.dm("send", bob.key+","+mallory.key, "the group secret")
	mallory.sync()
	group := mallory.inboxID("the group secret")

	if out, err := mallory.arc.Command(context.Background(), "message", "send", "--reply", group, bob.key, "look").CombinedOutput(); err == nil || !strings.Contains(string(out), "the reply goes to the members of its parent") {
		t.Errorf("arc sent a reply into another conversation: %v\n%s", err, out)
	}
	mallory.arcSend(bob.key, "look at this", "--arctest-tag", "e,"+group)
	bob.sync()
	conv := bob.dm("open", mallory.name)
	if !strings.Contains(conv, "\n↳ "+group[:6]+"  (in another conversation)\n  look at this\n") || strings.Contains(conv, "secret") {
		t.Errorf("bob's direct conversation with mallory:\n%s", conv)
	}
}

// A message with two e tags, or with an e tag that is no id, shows as a
// plain message. A relay hint after the id, as other NIP-17 clients write
// it, still makes a reply.
func TestATagThatIsNoReplyShowsAPlainMessage(t *testing.T) {
	t.Parallel()
	mallory, bob := newPerson(t, "mallory"), newPerson(t, "bob")
	mallory.dm("send", bob.key, "first")
	bob.sync()
	first := bob.inboxID("first")
	mallory.arcSend(bob.key, "two tags", "--arctest-tag", "e,"+first, "--arctest-tag", "e,"+first)
	mallory.arcSend(bob.key, "capital hex", "--arctest-tag", "e,"+strings.ToUpper(first))
	mallory.arcSend(bob.key, "with a hint", "--arctest-tag", "e,"+first+",wss://relay.example")
	bob.sync()
	conv := bob.dm("open", mallory.name)
	if got := quotes(conv); len(got) != 1 || got[0] != "↳ "+first[:6]+"  "+mallory.name+": first" {
		t.Errorf("the quote lines are %q, want one, for the reply with a hint:\n%s", got, conv)
	}
	if !strings.Contains(conv, "\n↳ "+first[:6]+"  "+mallory.name+": first\n  with a hint\n") {
		t.Errorf("the reply with a relay hint shows no quote:\n%s", conv)
	}
}

// arc-dm refuses an id that is not one, an id that names no message, and a
// reply that send would put into another conversation. arc refuses a
// parent that it does not hold, and an id in capitals. Nothing is sent.
func TestReplyRefusals(t *testing.T) {
	t.Parallel()
	ada, bob, cy := newPerson(t, "ada"), newPerson(t, "bob"), newPerson(t, "cy")
	ada.dm("send", bob.key, "from ada")
	cy.dm("send", bob.key, "from cy")
	bob.sync()
	fromAda := bob.inboxID("from ada")

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"reply", "zzzz", "x"}, `"zzzz" is not a message id`},
		{[]string{"reply", fromAda[:3], "x"}, "is not a message id"},
		{[]string{"reply", "ffffffff", "x"}, "no message ffffffff"},
		{[]string{"send", "-reply", fromAda[:6], cy.name, "x"}, fromAda[:6] + " is in another conversation"},
	} {
		if out := bob.refused(c.args...); !strings.Contains(out, c.want) {
			t.Errorf("arc-dm %s: %s, want %q", strings.Join(c.args, " "), out, c.want)
		}
	}
	for _, c := range []struct {
		reply string
		want  string
	}{
		{strings.Repeat("7a", 32), "no message " + strings.Repeat("7a", 32) + " on this device"},
		{strings.ToUpper(fromAda), "64 lowercase hex digits"},
	} {
		if out, err := bob.arc.Command(context.Background(), "message", "send", "--reply", c.reply, ada.key, "x").CombinedOutput(); err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("arc message send --reply %s: %v %s, want %q", c.reply, err, out, c.want)
		}
	}
	if sent := must(bob.arc.Outbox(context.Background())); len(sent) != 0 {
		t.Errorf("the refusals sent %d messages", len(sent))
	}
}

// A watch shows the short id and the quote of a reply, also when the
// parent is a message that this citizen sent after the watch started. A
// reply whose parent is not here shows the id only.
func TestAWatchQuotesTheParent(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "hello")
	bob.sync()

	watch := bob.command("watch", "-notify", "none")
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(waitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	bob.dm("send", ada.name, "bring the map")
	ada.sync()
	short := shortID(t, ada.dm("open", bob.name), "bring the map")
	ada.dm("reply", short, "yes")
	ada.arcSend(bob.key, "and that", "--arctest-tag", "e,"+strings.Repeat("7a", 32))
	want := []string{"\n↳ " + short + "  you: bring the map\n  yes\n", "\n↳ 7a7a7a7a7a7a  (not on this device)\n  and that\n"}
	if !arcprogramtest.WaitUntil(waitLimit, func() bool {
		return strings.Contains(stdout.String(), want[0]) && strings.Contains(stdout.String(), want[1])
	}) {
		t.Errorf("the watch printed:\n%s\nwant\n%s", stdout.String(), strings.Join(want, ""))
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
}

// The short id is six hex digits, and longer where two ids of this machine
// share their start. A prefix that names two messages is refused. A real
// collision of six digits costs about 2^24 hashes, too slow for a test run
// through the relay, so this test builds the store.
func TestShortIDs(t *testing.T) {
	t.Parallel()
	a := "abcdef01" + strings.Repeat("0", 56)
	b := "abcdef02" + strings.Repeat("0", 56)
	c := "123456" + strings.Repeat("0", 58)
	s := index([]conversation{{Peers: []string{"k"}, Entries: []entry{{ID: a}, {ID: b}, {ID: c}}}})
	for id, want := range map[string]string{a: "abcdef01", b: "abcdef02", c: "123456", strings.Repeat("9", 64): "999999"} {
		if got := s.short(id); got != want {
			t.Errorf("the short id of %s is %q, want %q", id[:8], got, want)
		}
	}
	if _, err := s.lookup("abcd"); err == nil || !strings.Contains(err.Error(), `"abcd" names 2 messages`) {
		t.Errorf("an ambiguous prefix: %v", err)
	}
	if got, err := s.lookup("abcdef02"); err != nil || got.ID != b {
		t.Errorf("lookup abcdef02 is %v, %v", got.ID, err)
	}
}

// A reply to a message that you sent yourself quotes your own text, on
// both sides: arc finds the parent in its outbox.
func TestAReplyToYourOwnMessage(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "bring the map")
	short := shortID(t, ada.dm("open", bob.key), "bring the map")
	if got, want := ada.dm("reply", short, "and the torch"), "replied to "+short+" in "+bob.name+"\n"; got != want {
		t.Errorf("arc-dm reply printed %q, want %q", got, want)
	}
	bob.sync()
	if conv := bob.dm("open", ada.name); !strings.Contains(conv, "\n↳ "+short+"  "+ada.name+": bring the map\n  and the torch\n") {
		t.Errorf("bob's conversation:\n%s", conv)
	}
	if conv := ada.dm("open", bob.name); !strings.Contains(conv, "\n↳ "+short+"  you: bring the map\n  and the torch\n") {
		t.Errorf("ada's conversation:\n%s", conv)
	}
}

// send -reply checks the id before it reads the text, so a wrong id fails
// at once, with standard input still open.
func TestSendReplyChecksTheIdBeforeTheText(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "hello")
	bob.sync()
	for _, c := range []struct {
		reply string
		want  string
	}{
		{"ffffffff", "no message ffffffff"},
		{"zz", "is not a message id"},
	} {
		command := bob.command("send", "-reply", c.reply, ada.name)
		stdin, open, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var out lockedBuffer
		command.Stdin, command.Stdout, command.Stderr = stdin, &out, &out
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		if !finish(t, command) {
			t.Errorf("send -reply %s waited for standard input:\n%s", c.reply, out.String())
		} else if !strings.Contains(out.String(), c.want) {
			t.Errorf("send -reply %s printed %q, want %q", c.reply, out.String(), c.want)
		}
		open.Close()
		stdin.Close()
	}
}

// A watch reads the outbox at most once for each parent that it does not
// hold, and at most once each refreshEvery, however many ids that no
// message has a sender names.
func TestAWatchReadsTheOutboxRarely(t *testing.T) {
	t.Parallel()
	mallory, bob := newPerson(t, "mallory"), newPerson(t, "bob")
	mallory.dm("send", bob.key, "hello")
	bob.sync()

	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	wrapper := filepath.Join(dir, "arc")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + runs + "\nexec " + bob.arc.Program + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	watch := exec.Command(env.program, "watch", "-arc", wrapper, "-home", bob.arc.Home, "-state", bob.state, "-notify", "none")
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(waitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	start := time.Now()
	const junk = 20
	for i := range junk {
		mallory.arcSend(bob.key, fmt.Sprintf("junk %d", i), "--arctest-tag", fmt.Sprintf("e,%064x", i+1))
	}
	if !arcprogramtest.WaitUntil(waitLimit, func() bool { return strings.Count(stdout.String(), "(not on this device)") == junk }) {
		t.Fatalf("the watch printed:\n%s", stdout.String())
	}
	elapsed := time.Since(start)
	data, _ := os.ReadFile(runs)
	// One read of the outbox at the start, then at most one each
	// refreshEvery, the first at once.
	most := 2 + int(elapsed/refreshEvery)
	if n := strings.Count(string(data), "message outbox"); n < 2 || n > most || n >= junk {
		t.Errorf("the watch read the outbox %d times in %v, want 2 to %d:\n%s", n, elapsed, most, data)
	}
	if n := strings.Count(string(data), "message inbox"); n != 1 {
		t.Errorf("the watch read the inbox %d times, want 1:\n%s", n, data)
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
}

// A flag after the peer is a flag, not text: Go's flag package stops at the
// first argument that is not a flag. A text that still starts with -reply
// is refused, and a word with a dash later in a text stays text.
func TestFlagsAfterThePeer(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "bring the map")
	bob.sync()
	short := shortID(t, bob.dm("open", ada.name), "bring the map")
	if got, want := bob.dm("send", ada.name, "-reply", short, "yes"), "replied to "+short+" in "+ada.name+"\n"; got != want {
		t.Errorf("send <peer> -reply printed %q, want %q", got, want)
	}
	if out := bob.refused("send", ada.name, "-reply=", "no"); !strings.Contains(out, `"" is not a message id`) {
		t.Errorf("an empty -reply: %s", out)
	}
	// After the peer, each word but -reply is text: the flags of arc-dm
	// do not take words of a message.
	texts := [][]string{
		{"--", "-reply", short, "is literal"},
		{"-5", "degrees"},
		{"-home", "is", "where", "the", "heart", "is"},
		{"-arc", "/no/such/arc", "everything"},
		{"-state", "of", "the", "art"},
		{"I", "think", "-v", "is", "fine"},
	}
	for _, words := range texts {
		bob.dm(append([]string{"send", ada.name}, words...)...)
	}
	// A text of only -h or --help is a request for help that came too
	// late: arc-dm refuses it, and after -- sends it.
	for _, help := range []string{"-h", "-help", "--help"} {
		if out := bob.refused("send", ada.name, help); !strings.Contains(out, "arc-dm send <peer> -- "+help) {
			t.Errorf("send <peer> %s: %s", help, out)
		}
	}
	bob.dm("send", ada.name, "--", "-h")
	if out := bob.refused("reply", short, "--help"); !strings.Contains(out, "arc-dm reply <id> -- --help") {
		t.Errorf("reply <id> --help: %s", out)
	}
	texts = append(texts, []string{"-h"})
	ada.sync()
	conv := ada.dm("open", bob.name)
	want := []string{"\n\u21b3 " + short + "  you: bring the map\n  yes\n", "\n  -reply " + short + " is literal\n"}
	for _, words := range texts[1:] {
		want = append(want, "\n  "+strings.Join(words, " ")+"\n")
	}
	for _, w := range want {
		if !strings.Contains(conv, w) {
			t.Errorf("ada's conversation holds no %q:\n%s", w, conv)
		}
	}
	if n := strings.Count(conv, "\n  "); n != 2+len(texts) {
		t.Errorf("ada's conversation holds %d texts, want %d: -reply= sent nothing:\n%s", n, 2+len(texts), conv)
	}
}

// A command checks the form of an id before it runs arc, so a wrong id
// fails even with no arc.
func TestAnIdIsCheckedBeforeArcRuns(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"reply", "zz", "hi"},
		{"send", "-reply", "zz", "ada", "hi"},
		{"send", "ada", "-reply=", "hi"},
	} {
		command := exec.Command(env.program, append([]string{args[0], "-arc", "/no/such/arc"}, args[1:]...)...)
		out, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "is not a message id") {
			t.Errorf("arc-dm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// growth is how many times longer f takes for 4n items than for n, the
// least of three tries. Time that grows with n log n gives about 4.5, and
// time that grows with the square of n gives 16.
func growth(n int, f func(n int)) float64 {
	measure := func(n int) time.Duration {
		least := time.Duration(1 << 62)
		for range 3 {
			start := time.Now()
			f(n)
			least = min(least, time.Since(start))
		}
		return least
	}
	return float64(measure(4*n)) / float64(measure(n))
}

// ids makes n message ids, out of order.
func ids(n int) []entry {
	return idsFrom(0, n)
}

// idsFrom makes n message ids that a hash makes from first, first+1, and
// on: like the ids of real messages, they share no start.
func idsFrom(first, n int) []entry {
	entries := make([]entry, n)
	for i := range entries {
		sum := sha256.Sum256([]byte(fmt.Sprint(first + i)))
		entries[i].ID = hex.EncodeToString(sum[:])
	}
	return entries
}

// The store adds a batch of messages, such as the outbox, with one sort,
// so a watch that reads a large outbox stays fast.
func TestAddIsNotQuadratic(t *testing.T) {
	if g := growth(25000, func(n int) {
		s := index(nil)
		for _, e := range ids(n) {
			s.add(e, conversation{})
		}
		s.short(s.ids[0])
	}); g > 9 {
		t.Errorf("4 times the messages took %.1f times as long, want under 9", g)
	}
}

// arc message send --reply checks the id, and that this machine holds it,
// before it reads standard input.
func TestArcChecksAReplyBeforeTheText(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	for _, c := range []struct{ reply, want string }{
		{"zz", "64 lowercase hex digits"},
		{strings.Repeat("7a", 32), "no message " + strings.Repeat("7a", 32) + " on this device"},
	} {
		command := bob.arc.Command(context.Background(), "message", "send", "--reply", c.reply, ada.key)
		stdin, open, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var out lockedBuffer
		command.Stdin, command.Stdout, command.Stderr = stdin, &out, &out
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		if !finish(t, command) {
			t.Errorf("arc message send --reply %s waited for standard input:\n%s", c.reply, out.String())
		} else if !strings.Contains(out.String(), c.want) {
			t.Errorf("arc message send --reply %s printed %q, want %q", c.reply, out.String(), c.want)
		}
		open.Close()
		stdin.Close()
	}
}

// The index of 200,000 messages takes one sort, not an insert for each
// id, whose time grows with the square of the number of messages.
func TestIndexIsNotQuadratic(t *testing.T) {
	if g := growth(25000, func(n int) { index([]conversation{{Peers: []string{"k"}, Entries: ids(n)}}) }); g > 9 {
		t.Errorf("4 times the messages took %.1f times as long, want under 9", g)
	}
	s := index([]conversation{{Peers: []string{"k"}, Entries: ids(1000)}})
	if sorted := s.sorted(); len(sorted) != 1000 || !slices.IsSorted(sorted) {
		t.Errorf("the index holds %d ids, sorted %v", len(sorted), slices.IsSorted(sorted))
	}
}

// A watch adds one message to its store in its place, with no sort: at
// 100,000 messages, a sort for each message took 2 to 3 percent of the
// time of a whole index, and an insert takes about 0.1 percent.
func TestALiveAddDoesNotSortTheStore(t *testing.T) {
	extra := idsFrom(1<<30, 200)
	ratio := 1.0
	for range 3 {
		start := time.Now()
		index([]conversation{{Peers: []string{"k"}, Entries: ids(100000)}})
		whole := time.Since(start)
		s := index([]conversation{{Peers: []string{"k"}, Entries: ids(100000)}})
		start = time.Now()
		for _, e := range extra {
			s.add(e, conversation{})
			s.short(e.ID)
		}
		ratio = min(ratio, float64(time.Since(start))/float64(len(extra))/float64(whole))
	}
	if ratio > 0.006 {
		t.Errorf("one message took %.4f of the time of a whole index, want under 0.006", ratio)
	}
}

// testWatcher is a watcher with an outbox that the test fills, and that
// records what it prints.
func testWatcher(outbox ...arcprogram.Sent) (w *watcher, printed *[]string, reads *int) {
	w, printed, reads, _ = testWatcherWith(outbox...)
	return w, printed, reads
}

// testWatcherWith is testWatcher, with the outbox that a test can change.
func testWatcherWith(outbox ...arcprogram.Sent) (w *watcher, printed *[]string, reads *int, sent *[]arcprogram.Sent) {
	printed, reads, sent = &[]string{}, new(int), &outbox
	w = &watcher{
		outbox: func(context.Context) ([]arcprogram.Sent, error) {
			*reads++
			return *sent, nil
		},
		me:       "me",
		messages: index(nil),
		missed:   map[string]bool{},
		waiting:  map[string]int{},
		last:     time.Now(),
	}
	w.print = func(m arcprogram.Message, messages *store) {
		e := entry{ID: m.ID, Text: m.Text, ReplyTo: m.ReplyTo}
		*printed = append(*printed, m.Text+" "+messages.quote(e, conversation{Peers: []string{"peer"}}))
	}
	return w, printed, reads, sent
}

// msg is a message of the tests of the watcher: one per id, from a sender.
func msg(id int, from, text, replyTo string) arcprogram.Message {
	return arcprogram.Message{ID: idsFrom(id, 1)[0].ID, Text: text, ReplyTo: replyTo, From: from, Members: []string{from, "me"}}
}

// sentParent is a message of this citizen in the outbox, to "peer".
func sentParent(id, text string) arcprogram.Sent {
	return arcprogram.Sent{ID: id, To: "peer", Text: text, Members: []string{"me", "peer"}}
}

// The watch holds at most maxQueue messages: a burst past that prints them
// at once, in order, and loses none.
func TestTheHeldQueueIsCapped(t *testing.T) {
	w, printed, _ := testWatcher()
	w.take(msg(0, "peer", "junk", strings.Repeat("7a", 32)))
	for i := range 3 * maxQueue {
		w.take(msg(i+1, "peer", fmt.Sprint(i), ""))
		if len(w.queue) > maxQueue {
			t.Fatalf("the watch holds %d messages, want at most %d", len(w.queue), maxQueue)
		}
	}
	w.close()
	if len(*printed) != 3*maxQueue+1 || !strings.HasPrefix((*printed)[0], "junk ") || !strings.HasPrefix((*printed)[3*maxQueue], fmt.Sprint(3*maxQueue-1)+" ") {
		t.Errorf("the watch printed %d messages, want %d in order", len(*printed), 3*maxQueue+1)
	}
}

// Cato's case: one junk reply, one valid reply whose parent is in the
// outbox, then 254 junk replies of the same sender. The junk of one sender
// past its quota prints at once, and the valid reply still gets its quote.
func TestOneSendersJunkDoesNotHideAValidReply(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, reads := testWatcher(sentParent(parent, "bring the map"))
	w.take(msg(0, "mallory", "junk", strings.Repeat("7a", 32)))
	w.take(msg(1, "peer", "yes", parent))
	for i := range 254 {
		w.take(msg(i+2, "mallory", "junk", idsFrom(1000+i, 1)[0].ID))
	}
	w.close()
	if !slices.Contains(*printed, "yes \u21b3 ababab  you: bring the map") || *reads == 0 {
		t.Errorf("after %d reads the watch printed %q, want the quote of the valid reply", *reads, *printed)
	}
	if len(*printed) != 256 {
		t.Errorf("the watch printed %d messages, want 256", len(*printed))
	}
}

// A busy group, 600 messages behind a valid reply that waits, does not
// push the reply out unquoted: only replies that wait count toward the
// cap, and the order stays.
func TestABusyGroupDoesNotHideAValidReply(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _ := testWatcher(sentParent(parent, "bring the map"))
	w.take(msg(0, "peer", "yes", parent))
	for i := range 600 {
		w.take(msg(i+1, "cy", fmt.Sprint(i), ""))
	}
	w.close()
	if len(*printed) != 601 || (*printed)[0] != "yes \u21b3 ababab  you: bring the map" || !strings.HasPrefix((*printed)[600], "599 ") {
		t.Errorf("the watch printed %d messages, first %q, want the quoted reply first", len(*printed), (*printed)[0])
	}
}

// An overflow that prints without a read of the outbox marks no parent
// missed, so a later reply to it, once the parent is in the outbox, still
// gets its quote.
func TestAnOverflowMarksNoParentMissed(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _, sent := testWatcherWith()
	senders := maxWaiting / maxWaitingPerSender
	for i := range maxWaitingPerSender {
		for k := range senders {
			w.take(msg(i*1000+k, fmt.Sprint("sender", k), "early", parent))
		}
	}
	if len(w.queue) != 0 {
		t.Fatalf("after %d waiting replies the watch still holds %d", maxWaiting, len(w.queue))
	}
	*sent = append(*sent, sentParent(parent, "bring the map"))
	w.take(msg(99999, "peer", "late", parent))
	w.close()
	if last := (*printed)[len(*printed)-1]; last != "late \u21b3 ababab  you: bring the map" {
		t.Errorf("the late reply printed %q, want its quote", last)
	}
}

// At its end, the watch reads the outbox once for a reply that waits, so
// a parent that this citizen sent shows in the quote.
func TestTheEndOfAWatchReadsTheOutbox(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, reads := testWatcher(arcprogram.Sent{ID: parent, To: "peer", Text: "bring the map", Members: []string{"me", "peer"}})
	w.take(arcprogram.Message{ID: strings.Repeat("cd", 32), Text: "yes", ReplyTo: parent, From: "peer", Members: []string{"peer", "me"}})
	if len(*printed) != 0 {
		t.Fatalf("the reply printed before a read: %q", *printed)
	}
	w.close()
	if want := "yes \u21b3 ababab  you: bring the map"; len(*printed) != 1 || (*printed)[0] != want || *reads != 1 {
		t.Errorf("the end of the watch printed %q after %d reads, want %q after 1", *printed, *reads, want)
	}
}

// A flood of messages that a watch adds one at a time takes time that
// grows with their number, not its square.
func TestALiveFloodIsLinear(t *testing.T) {
	if g := growth(25000, func(n int) {
		s := index(nil)
		for _, e := range ids(n) {
			s.add(e, conversation{})
			s.short(e.ID)
		}
	}); g > 9 {
		t.Errorf("4 times the messages took %.1f times as long, want under 9", g)
	}
}

// An overflow reads the outbox when the last read is older than
// overflowRead, so the replies that wait get their quotes.
func TestAnOverflowReadsAnOldOutbox(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, reads := testWatcher(sentParent(parent, "bring the map"))
	w.last = time.Now().Add(-2 * overflowRead)
	defer w.close()
	for i := range maxWaitingPerSender {
		for k := range maxWaiting / maxWaitingPerSender {
			w.take(msg(i*1000+k, fmt.Sprint("sender", k), "yes", parent))
		}
	}
	if *reads != 1 || len(*printed) != maxWaiting || (*printed)[0] != "yes \u21b3 ababab  you: bring the map" {
		t.Errorf("after %d reads the watch printed %d messages, the first %q", *reads, len(*printed), (*printed)[0])
	}
}

// A second interrupt ends the watch at once, even while its end waits on
// an arc that hangs.
func TestASecondInterruptEndsTheWatch(t *testing.T) {
	t.Parallel()
	mallory, bob := newPerson(t, "mallory"), newPerson(t, "bob")
	mallory.dm("send", bob.key, "hello")
	bob.sync()
	dir := t.TempDir()
	count, wrapper := filepath.Join(dir, "count"), filepath.Join(dir, "arc")
	// The second read of the outbox, at the end of the watch, hangs.
	script := "#!/bin/sh\ncase " + `"$*"` + " in *" + `"message outbox"` + "*) echo x >> " + count + "; [ $(wc -l < " + count + ") -ge 2 ] && exec sleep 30;; esac\nexec " + bob.arc.Program + " " + `"$@"` + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	watch := exec.Command(env.program, "watch", "-arc", wrapper, "-home", bob.arc.Home, "-state", bob.state, "-notify", "none")
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(waitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	mallory.arcSend(bob.key, "junk", "--arctest-tag", "e,"+strings.Repeat("7a", 32))
	time.Sleep(500 * time.Millisecond)
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	start := time.Now()
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = watch.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeRead - 2*time.Second):
		t.Fatalf("the watch went on %v after a second interrupt", time.Since(start))
	}
}

// pairs makes 2n ids in pairs that share their first 10 digits and differ
// at the 11th, as a
// sender who spent 2^40 hashes on each pair could make them.
func pairs(n int) []entry {
	var out []entry
	for _, e := range idsFrom(1<<20, n) {
		// The twin differs at the 11th digit.
		eleventh := map[bool]string{true: "1", false: "0"}[e.ID[10] == '0']
		twin := e.ID[:10] + eleventh + idsFrom(1<<21+len(out), 1)[0].ID[11:]
		out = append(out, e, entry{ID: twin})
	}
	return out
}

// Ids that share 8 digits take their short id from their bucket: it is
// long enough to name one message, and a flood of such pairs stays linear.
func TestCraftedCollisionsStayFast(t *testing.T) {
	s := index([]conversation{{Peers: []string{"k"}, Entries: pairs(10)}})
	for _, e := range pairs(10) {
		if short := s.short(e.ID); len(short) != 11 || short != e.ID[:11] {
			t.Errorf("the short id of %s is %q, want its first 11 digits", e.ID[:12], short)
		}
	}
	if g := growth(10000, func(n int) {
		s := index(nil)
		for _, e := range pairs(n) {
			s.add(e, conversation{})
			s.short(e.ID)
		}
	}); g > 9 {
		t.Errorf("4 times the pairs took %.1f times as long, want under 9", g)
	}
}

// A watch remembers at most maxMissed ids that a read of the outbox did
// not find.
func TestTheWatchForgetsMissedIds(t *testing.T) {
	w := &watcher{missed: map[string]bool{}}
	for i := range 2 * maxMissed {
		w.miss(fmt.Sprint(i))
	}
	if len(w.missed) > maxMissed {
		t.Errorf("the watch remembers %d ids, want at most %d", len(w.missed), maxMissed)
	}
}

// A parent of only spaces, such as U+3000, quotes as (blank). arc refuses
// to send one, so no relay test can make it; another client can.
func TestABlankParentQuotesAsBlank(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"\u3000\u3000\u3000": "(blank)",
		" \n\t\n":            "(blank)",
		"\u3000\u3000\nreal": "real",
		"real\nmore":         "real \u2026",
	} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// A reply whose parent the watch does not hold waits for the next read of
// the outbox, and is not shown without its parent. A reply to an id that
// no message has, then within 5 s a reply to a message that bob sent from
// another process after the watch started: the second still shows its
// parent, and the replies keep their order.
func TestAWatchHoldsAReplyUntilItReadsTheOutbox(t *testing.T) {
	t.Parallel()
	ada, bob, mallory := newPerson(t, "ada"), newPerson(t, "bob"), newPerson(t, "mallory")
	ada.dm("send", bob.key, "hello")
	mallory.dm("send", bob.key, "hello too")
	bob.sync()

	watch := bob.command("watch", "-notify", "none")
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(waitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	mallory.arcSend(bob.key, "junk", "--arctest-tag", "e,"+strings.Repeat("7a", 32))
	bob.dm("send", ada.name, "bring the map")
	ada.sync()
	short := shortID(t, ada.dm("open", bob.name), "bring the map")
	ada.dm("reply", short, "yes")
	want := "\n\u21b3 " + short + "  you: bring the map\n  yes\n"
	if !arcprogramtest.WaitUntil(waitLimit, func() bool { return strings.Contains(stdout.String(), want) }) {
		t.Fatalf("the watch printed:\n%s\nwant\n%s", stdout.String(), want)
	}
	if out := stdout.String(); strings.Index(out, "  junk\n") > strings.Index(out, want) || strings.Contains(out, short+"  (not on this device)") {
		t.Errorf("the watch printed:\n%s", out)
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
}

// A reply that waits for the outbox when the watch ends still shows its
// parent: the watch reads the outbox once more as it ends.
func TestAWatchShowsAHeldReplyAtItsEnd(t *testing.T) {
	t.Parallel()
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "hello")
	bob.sync()

	watch := bob.command("watch", "-notify", "none")
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(waitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	started := time.Now()
	bob.dm("send", ada.name, "bring the map")
	ada.sync()
	short := shortID(t, ada.dm("open", bob.name), "bring the map")
	ada.dm("reply", short, "yes")
	// The watch takes the reply within a moment, and holds it until 5 s
	// after it started. The test ends the watch before then, when it can.
	time.Sleep(500 * time.Millisecond)
	if time.Since(started) > refreshEvery {
		t.Logf("the machine is slow: the watch read the outbox before its end, %v after its start", time.Since(started))
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
	if want := "\n\u21b3 " + short + "  you: bring the map\n  yes\n"; !strings.Contains(stdout.String(), want) {
		t.Errorf("the watch printed:\n%s\nwant\n%s", stdout.String(), want)
	}
}
