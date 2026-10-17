package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gezibash/arc/sdk/arcprogram/arcprogramtest"
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
	mallory, bob := newPerson(t, "mallory"), newPerson(t, "bob")
	parents := []string{
		"\x1b[2Jred\u202eevil\u200b" + strings.Repeat("x", 5000) + "\nsecond line",
		"\n\n\u200b\u2060\nlast",
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
		if n := utf8.RuneCountInString(line); n > 72 {
			t.Errorf("a quote line holds %d characters, want at most 72: %q", n, line)
		}
		if strings.Contains(line, "(not on this device)") {
			nearQuote = append(nearQuote, line)
		} else {
			got = append(got, line)
		}
	}
	if len(got) != 2 {
		t.Fatalf("the quote lines are %q, want two", got)
	}
	if want := "↳ " + held[:12] + "…  (not on this device)"; len(nearQuote) != 1 || nearQuote[0] != want {
		t.Errorf("the quote of the near id is %q, want %q", nearQuote, want)
	}
	if n := utf8.RuneCountInString(got[0]); n > 72 || !strings.Contains(got[0], `: \x1b[2Jred\u202eevil\u200bxxx`) || !strings.HasSuffix(got[0], "…") {
		t.Errorf("the quote of the long parent is %q (%d characters), want it escaped and cut to 72", got[0], n)
	}
	if !strings.HasSuffix(got[1], `: \u200b\u2060 …`) {
		t.Errorf("the quote of the parent with empty lines is %q", got[1])
	}
}

// A reply to a message that the reader does not hold shows the id only.
func TestAReplyToAnUnheldMessageShowsTheIdOnly(t *testing.T) {
	mallory, bob := newPerson(t, "mallory"), newPerson(t, "bob")
	unknown := strings.Repeat("7a", 32)
	mallory.arcSend(bob.key, "you said so", "--arctest-tag", "e,"+unknown)
	bob.sync()
	if conv := bob.dm("open", mallory.name); !strings.Contains(conv, "\n↳ 7a7a7a  (not on this device)\n  you said so\n") {
		t.Errorf("bob's conversation:\n%s", conv)
	}
}

// A reply that names a message of another conversation shows no text of
// it: a member of a group cannot move the words of the group into a direct
// conversation, and an honest arc refuses to send such a reply.
func TestAReplyAcrossConversationsShowsNoText(t *testing.T) {
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
	if !arcprogramtest.WaitUntil(15*time.Second, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	bob.dm("send", ada.name, "bring the map")
	ada.sync()
	short := shortID(t, ada.dm("open", bob.name), "bring the map")
	ada.dm("reply", short, "yes")
	ada.arcSend(bob.key, "and that", "--arctest-tag", "e,"+strings.Repeat("7a", 32))
	want := []string{"\n↳ " + short + "  you: bring the map\n  yes\n", "\n↳ 7a7a7a  (not on this device)\n  and that\n"}
	if !arcprogramtest.WaitUntil(15*time.Second, func() bool {
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
// hold, however many replies name that parent: a sender can name many
// ids that no message has.
func TestAWatchReadsTheOutboxOncePerMissingParent(t *testing.T) {
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
	if !arcprogramtest.WaitUntil(15*time.Second, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	one, two := strings.Repeat("7a", 32), strings.Repeat("7b", 32)
	for i, junk := range []string{one, one, one, two} {
		mallory.arcSend(bob.key, "junk "+string(rune('a'+i)), "--arctest-tag", "e,"+junk)
	}
	if !arcprogramtest.WaitUntil(15*time.Second, func() bool { return strings.Count(stdout.String(), "(not on this device)") == 4 }) {
		t.Fatalf("the watch printed:\n%s", stdout.String())
	}
	data, _ := os.ReadFile(runs)
	// One read of the outbox at the start, then one for each missing id.
	if n := strings.Count(string(data), "message outbox"); n != 3 {
		t.Errorf("the watch read the outbox %d times, want 3:\n%s", n, data)
	}
	if n := strings.Count(string(data), "message inbox"); n != 1 {
		t.Errorf("the watch read the inbox %d times, want 1:\n%s", n, data)
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
}
