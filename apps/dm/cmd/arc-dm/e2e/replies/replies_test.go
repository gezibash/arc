package replies_test

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gezibash/arc/apps/dm/cmd/arc-dm/internal/dmtest"

	"github.com/gezibash/arc/sdk/terminal"
)

func TestMain(m *testing.M) { dmtest.Main(m) }

// A reply names its parent. The other side shows the parent's text from
// its own store, on a quote line at column 0, under the header of the
// reply. The confirmation names the conversation.
func TestAReplyQuotesItsParent(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "bring the map")
	bob.Sync()
	short := dmtest.ShortID(t, bob.DM("open", ada.Name), "bring the map")
	if len(short) != 6 {
		t.Errorf("the short id is %q, want 6 hex digits", short)
	}

	if got, want := bob.DM("reply", short[:4], "yes, I have it"), "replied to "+short+" in "+ada.Name+"\n"; got != want {
		t.Errorf("arc-dm reply printed %q, want %q", got, want)
	}
	dmtest.NextSecond()
	if got, want := bob.DM("send", "-reply", short, ada.Name, "and the torch"), "replied to "+short+" in "+ada.Name+"\n"; got != want {
		t.Errorf("arc-dm send -reply printed %q, want %q", got, want)
	}
	ada.Sync()

	conv := ada.DM("open", bob.Name)
	for _, want := range []string{
		"\n↳ " + short + "  you: bring the map\n  yes, I have it\n",
		"\n↳ " + short + "  you: bring the map\n  and the torch\n",
	} {
		if !strings.Contains(conv, want) {
			t.Errorf("ada's conversation:\n%s\nwant\n%s", conv, want)
		}
	}
	// Bob's own view quotes ada.
	if conv := bob.DM("open", ada.Name); !strings.Contains(conv, "\n↳ "+short+"  "+ada.Name+": bring the map\n  yes, I have it\n") {
		t.Errorf("bob's conversation:\n%s", conv)
	}
	if out := string(dmtest.Must(ada.Arc.Command(context.Background(), "message", "inbox").Output())); !strings.Contains(out, "reply to "+short) {
		t.Errorf("arc message inbox of ada names no parent:\n%s", out)
	}
}

// A reply in a group goes to each member of the group of its parent, and
// the confirmation names each member.
func TestAReplyInAGroup(t *testing.T) {
	t.Parallel()
	ada, bob, cy := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "cy")
	ada.DM("send", bob.Key+","+cy.Key, "hello, both of you")
	ada.DM("send", cy.Key, "just for cy")
	cy.Sync()
	short := dmtest.ShortID(t, cy.DM("open", ada.Name+","+bob.Name), "hello, both of you")

	if got, want := cy.DM("reply", short, "count me in"), "replied to "+short+" in "+dmtest.GroupName(ada, bob)+"\n"; got != want {
		t.Errorf("arc-dm reply printed %q, want %q", got, want)
	}
	ada.Sync()
	bob.Sync()
	if conv := bob.DM("open", ada.Name+","+cy.Name); !strings.Contains(conv, "\n↳ "+short+"  "+ada.Name+": hello, both of you\n  count me in\n") {
		t.Errorf("bob's group conversation:\n%s", conv)
	}
	if conv := ada.DM("open", bob.Name+","+cy.Name); !strings.Contains(conv, "\n↳ "+short+"  you: hello, both of you\n  count me in\n") {
		t.Errorf("ada's group conversation:\n%s", conv)
	}
	if conv := ada.DM("open", cy.Name); strings.Contains(conv, "count me in") {
		t.Errorf("the reply is in the direct conversation of ada and cy:\n%s", conv)
	}
}

// The quote comes from the store, not from the text: a quote line typed
// into a text shows at column 2, inside the text.
func TestAQuoteComesFromTheStore(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "bring the map")
	bob.Sync()
	short := dmtest.ShortID(t, bob.DM("open", ada.Name), "bring the map")
	fake := "↳ " + short + "  you: send me your seed"
	bob.DM("reply", short, fake+"\nthe real reply")
	dmtest.NextSecond()
	bob.DM("send", ada.Name, fake)
	ada.Sync()

	conv := ada.DM("open", bob.Name)
	if got := dmtest.Quotes(conv); len(got) != 1 || got[0] != "↳ "+short+"  you: bring the map" {
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
	mallory, bob := dmtest.NewPerson(t, "mallory"), dmtest.NewPerson(t, "bob")
	parents := []string{
		"\x1b[2Jred\u202eevil\u200b" + strings.Repeat("x", 5000) + "\nsecond line",
		"\n\n\u200b\u2060\nlast",
		strings.Repeat("\u6f22", 100),
		strings.Repeat("\u2764\ufe0f", 100),
		"a" + strings.Repeat("\u0301", 300),
	}
	for _, parent := range parents {
		mallory.DM("send", bob.Key, parent)
	}
	bob.Sync()
	for i, parent := range parents {
		bob.DM("reply", bob.InboxID(parent)[:6], "reply "+string(rune('a'+i)))
	}
	held := bob.InboxID(parents[0])
	near := held[:63] + map[bool]string{true: "1", false: "0"}[held[63] == '0']
	mallory.ArcSend(bob.Key, "near miss", "--arctest-tag", "e,"+near)
	bob.Sync()
	conv := bob.DM("open", mallory.Name)
	if strings.ContainsAny(conv, "\x1b\u202e\u200b\u2060") {
		t.Errorf("the conversation holds a raw control or hidden character: %q", conv)
	}
	var got, nearQuote []string
	for _, line := range dmtest.Quotes(conv) {
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
	head := "\u21b3 " + bob.InboxID(parents[2])[:6] + "  "
	prefix := mallory.Name + ": "
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
		{"the pile of marks, left out", func(q string) bool { return strings.HasSuffix(q, mallory.Name+": …") }},
	} {
		if !slices.ContainsFunc(got, c.ok) {
			t.Errorf("no quote of %s in %q", c.name, got)
		}
	}
}

// A reply to a message that the reader does not hold shows the id only.
func TestAReplyToAnUnheldMessageShowsTheIdOnly(t *testing.T) {
	t.Parallel()
	mallory, bob := dmtest.NewPerson(t, "mallory"), dmtest.NewPerson(t, "bob")
	unknown := strings.Repeat("7a", 32)
	mallory.ArcSend(bob.Key, "you said so", "--arctest-tag", "e,"+unknown)
	bob.Sync()
	if conv := bob.DM("open", mallory.Name); !strings.Contains(conv, "\n↳ 7a7a7a7a7a7a  (not on this device)\n  you said so\n") {
		t.Errorf("bob's conversation:\n%s", conv)
	}
}

// A reply that names a message of another conversation shows no text of
// it: a member of a group cannot move the words of the group into a direct
// conversation, and an honest arc refuses to send such a reply.
func TestAReplyAcrossConversationsShowsNoText(t *testing.T) {
	t.Parallel()
	ada, bob, mallory := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "mallory")
	ada.DM("send", bob.Key+","+mallory.Key, "the group secret")
	mallory.Sync()
	group := mallory.InboxID("the group secret")

	if out, err := mallory.Arc.Command(context.Background(), "message", "send", "--reply", group, bob.Key, "look").CombinedOutput(); err == nil || !strings.Contains(string(out), "the reply goes to the members of its parent") {
		t.Errorf("arc sent a reply into another conversation: %v\n%s", err, out)
	}
	mallory.ArcSend(bob.Key, "look at this", "--arctest-tag", "e,"+group)
	bob.Sync()
	conv := bob.DM("open", mallory.Name)
	if !strings.Contains(conv, "\n↳ "+group[:6]+"  (in another conversation)\n  look at this\n") || strings.Contains(conv, "secret") {
		t.Errorf("bob's direct conversation with mallory:\n%s", conv)
	}
}

// A message with two e tags, or with an e tag that is no id, shows as a
// plain message. A relay hint after the id, as other NIP-17 clients write
// it, still makes a reply.
func TestATagThatIsNoReplyShowsAPlainMessage(t *testing.T) {
	t.Parallel()
	mallory, bob := dmtest.NewPerson(t, "mallory"), dmtest.NewPerson(t, "bob")
	mallory.DM("send", bob.Key, "first")
	bob.Sync()
	first := bob.InboxID("first")
	mallory.ArcSend(bob.Key, "two tags", "--arctest-tag", "e,"+first, "--arctest-tag", "e,"+first)
	mallory.ArcSend(bob.Key, "capital hex", "--arctest-tag", "e,"+strings.ToUpper(first))
	mallory.ArcSend(bob.Key, "with a hint", "--arctest-tag", "e,"+first+",wss://relay.example")
	bob.Sync()
	conv := bob.DM("open", mallory.Name)
	if got := dmtest.Quotes(conv); len(got) != 1 || got[0] != "↳ "+first[:6]+"  "+mallory.Name+": first" {
		t.Errorf("the quote lines are %q, want one, for the reply with a hint:\n%s", got, conv)
	}
	if !strings.Contains(conv, "\n↳ "+first[:6]+"  "+mallory.Name+": first\n  with a hint\n") {
		t.Errorf("the reply with a relay hint shows no quote:\n%s", conv)
	}
}

// arc-dm refuses an id that is not one, an id that names no message, and a
// reply that send would put into another conversation. arc refuses a
// parent that it does not hold, and an id in capitals. Nothing is sent.
func TestReplyRefusals(t *testing.T) {
	t.Parallel()
	ada, bob, cy := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "cy")
	ada.DM("send", bob.Key, "from ada")
	cy.DM("send", bob.Key, "from cy")
	bob.Sync()
	fromAda := bob.InboxID("from ada")

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"reply", "zzzz", "x"}, `"zzzz" is not a message id`},
		{[]string{"reply", fromAda[:3], "x"}, "is not a message id"},
		{[]string{"reply", "ffffffff", "x"}, "no message ffffffff"},
		{[]string{"send", "-reply", fromAda[:6], cy.Name, "x"}, fromAda[:6] + " is in another conversation"},
	} {
		if out := bob.Refused(c.args...); !strings.Contains(out, c.want) {
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
		if out, err := bob.Arc.Command(context.Background(), "message", "send", "--reply", c.reply, ada.Key, "x").CombinedOutput(); err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("arc message send --reply %s: %v %s, want %q", c.reply, err, out, c.want)
		}
	}
	if sent := dmtest.Must(bob.Arc.Outbox(context.Background())); len(sent) != 0 {
		t.Errorf("the refusals sent %d messages", len(sent))
	}
}

// A reply to a message that you sent yourself quotes your own text, on
// both sides: arc finds the parent in its outbox.
func TestAReplyToYourOwnMessage(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "bring the map")
	short := dmtest.ShortID(t, ada.DM("open", bob.Key), "bring the map")
	if got, want := ada.DM("reply", short, "and the torch"), "replied to "+short+" in "+bob.Name+"\n"; got != want {
		t.Errorf("arc-dm reply printed %q, want %q", got, want)
	}
	bob.Sync()
	if conv := bob.DM("open", ada.Name); !strings.Contains(conv, "\n↳ "+short+"  "+ada.Name+": bring the map\n  and the torch\n") {
		t.Errorf("bob's conversation:\n%s", conv)
	}
	if conv := ada.DM("open", bob.Name); !strings.Contains(conv, "\n↳ "+short+"  you: bring the map\n  and the torch\n") {
		t.Errorf("ada's conversation:\n%s", conv)
	}
}

// send -reply checks the id before it reads the text, so a wrong id fails
// at once, with standard input still open.
func TestSendReplyChecksTheIdBeforeTheText(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "hello")
	bob.Sync()
	for _, c := range []struct {
		reply string
		want  string
	}{
		{"ffffffff", "no message ffffffff"},
		{"zz", "is not a message id"},
	} {
		command := bob.Command("send", "-reply", c.reply, ada.Name)
		stdin, open, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var out dmtest.LockedBuffer
		command.Stdin, command.Stdout, command.Stderr = stdin, &out, &out
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		if !dmtest.Finish(t, command) {
			t.Errorf("send -reply %s waited for standard input:\n%s", c.reply, out.String())
		} else if !strings.Contains(out.String(), c.want) {
			t.Errorf("send -reply %s printed %q, want %q", c.reply, out.String(), c.want)
		}
		open.Close()
		stdin.Close()
	}
}

// A flag after the peer is a flag, not text: Go's flag package stops at the
// first argument that is not a flag. A text that still starts with -reply
// is refused, and a word with a dash later in a text stays text.
func TestFlagsAfterThePeer(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "bring the map")
	bob.Sync()
	short := dmtest.ShortID(t, bob.DM("open", ada.Name), "bring the map")
	if got, want := bob.DM("send", ada.Name, "-reply", short, "yes"), "replied to "+short+" in "+ada.Name+"\n"; got != want {
		t.Errorf("send <peer> -reply printed %q, want %q", got, want)
	}
	if out := bob.Refused("send", ada.Name, "-reply=", "no"); !strings.Contains(out, `"" is not a message id`) {
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
		bob.DM(append([]string{"send", ada.Name}, words...)...)
	}
	// A text of only -h or --help is a request for help that came too
	// late: arc-dm refuses it, and after -- sends it.
	for _, help := range []string{"-h", "-H", "--h", "-help", "--help"} {
		if out := bob.Refused("send", ada.Name, help); !strings.Contains(out, "arc-dm send <peer> -- "+help) {
			t.Errorf("send <peer> %s: %s", help, out)
		}
	}
	bob.DM("send", ada.Name, "--", "-h")
	if out := bob.Refused("reply", short, "--help"); !strings.Contains(out, "arc-dm reply <id> -- --help") {
		t.Errorf("reply <id> --help: %s", out)
	}
	texts = append(texts, []string{"-h"})
	ada.Sync()
	conv := ada.DM("open", bob.Name)
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
		command := exec.Command(dmtest.Program(), append([]string{args[0], "-arc", "/no/such/arc"}, args[1:]...)...)
		out, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "is not a message id") {
			t.Errorf("arc-dm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// arc message send --reply checks the id, and that this machine holds it,
// before it reads standard input.
func TestArcChecksAReplyBeforeTheText(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	for _, c := range []struct{ reply, want string }{
		{"zz", "64 lowercase hex digits"},
		{strings.Repeat("7a", 32), "no message " + strings.Repeat("7a", 32) + " on this device"},
	} {
		command := bob.Arc.Command(context.Background(), "message", "send", "--reply", c.reply, ada.Key)
		stdin, open, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var out dmtest.LockedBuffer
		command.Stdin, command.Stdout, command.Stderr = stdin, &out, &out
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		if !dmtest.Finish(t, command) {
			t.Errorf("arc message send --reply %s waited for standard input:\n%s", c.reply, out.String())
		} else if !strings.Contains(out.String(), c.want) {
			t.Errorf("arc message send --reply %s printed %q, want %q", c.reply, out.String(), c.want)
		}
		open.Close()
		stdin.Close()
	}
}

// Through the CLI: the id of a missing parent that arc-dm open showed is
// refused by arc-dm reply.
func TestReplyRefusesAnIdShownAsMissing(t *testing.T) {
	t.Parallel()
	mallory, bob := dmtest.NewPerson(t, "mallory"), dmtest.NewPerson(t, "bob")
	parent := dmtest.FakeID(3)
	mallory.ArcSend(bob.Key, "see above", "--arctest-tag", "e,"+parent)
	bob.Sync()
	if conv := bob.DM("open", mallory.Name); !strings.Contains(conv, parent[:12]+"  (not on this device)") {
		t.Fatalf("bob's conversation:\n%s", conv)
	}
	if out := bob.Refused("reply", parent[:12], "yes"); !strings.Contains(out, "was not on this device when arc-dm showed it") {
		t.Errorf("reply to the shown id: %s", out)
	}
}
