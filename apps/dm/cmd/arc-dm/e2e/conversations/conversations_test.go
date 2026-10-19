package conversations_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gezibash/arc/apps/dm/cmd/arc-dm/internal/dmtest"

	"github.com/gezibash/arc/sdk/arcprogram/arcprogramtest"
)

func TestMain(m *testing.M) { dmtest.Main(m) }

// TestConversations groups the messages by peer, with the replies of this
// citizen, and counts the unread messages until the citizen opens them.
func TestConversations(t *testing.T) {
	t.Parallel()
	ada, bob, cy := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "cy")

	ada.DM("send", bob.Key, "meet at noon")
	dmtest.NextSecond()
	ada.DM("send", bob.Key, "bring the map")
	// A message that arc message sent, and not arc-dm, is in the conversation too.
	if _, err := cy.Arc.Send(context.Background(), []string{bob.Key}, "hello bob"); err != nil {
		t.Fatal(err)
	}
	dmtest.NextSecond()
	bob.Sync()
	bob.DM("send", ada.Name, "see you there")

	got := bob.List()
	want := []dmtest.Block{
		{Header: "  " + ada.Name + "  2 unread", Preview: "you: see you there"},
		{Header: "  " + cy.Name + "  1 unread", Preview: "hello bob"},
	}
	if len(got) != len(want) {
		t.Fatalf("bob has %d conversations, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		// The header starts with the time of the newest message.
		if !strings.HasSuffix(got[i].Header, want[i].Header) || got[i].Preview != want[i].Preview {
			t.Errorf("conversation %d is %q, want %q after the time", i, got[i], want[i])
		}
	}

	conv := bob.DM("open", ada.Name)
	order := []string{
		ada.Name + "  new\n  meet at noon\n",
		ada.Name + "  new\n  bring the map\n",
		"you  pending\n  see you there\n",
	}
	dmtest.AssertInOrder(t, conv, order)

	got = bob.List()
	if strings.Contains(got[0].Header, "unread") || !strings.HasSuffix(got[1].Header, cy.Name+"  1 unread") {
		t.Fatalf("after bob opened ada, the conversations are %q; want ada read and cy with 1 unread", got)
	}
	// "  new" ends the header of an unread message. A petname can hold
	// "new", as in fluid-newcomb, but never two spaces.
	if again := bob.DM("open", ada.Name); strings.Contains(again, "  new\n") {
		t.Errorf("the second open still shows new messages:\n%s", again)
	}

	// A message that arrives after the open is unread.
	dmtest.NextSecond()
	ada.DM("send", bob.Key, "one more thing")
	bob.Sync()
	got = bob.List()
	if !strings.HasSuffix(got[0].Header, ada.Name+"  1 unread") || got[0].Preview != "one more thing" {
		t.Errorf("after a new message, ada's conversation is %q, want 1 unread", got[0])
	}

	bob.DM("read")
	for _, b := range bob.List() {
		if strings.Contains(b.Header, "unread") {
			t.Errorf("after arc-dm read, %q is still unread", b.Header)
		}
	}
}

// TestGroups sends one message to two peers. Each member sees one
// conversation of the three, a reply goes to each member, and the sender
// sees to how many members the message was delivered. A direct
// conversation between two of them stays apart.
func TestGroups(t *testing.T) {
	t.Parallel()
	ada, bob, cy := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "cy")

	ada.DM("send", bob.Key+","+cy.Key, "hello, both of you")
	dmtest.NextSecond()
	ada.DM("send", bob.Key, "just for bob")
	bob.Sync()
	ada.Sync()

	// Bob acknowledged; cy did not sync yet.
	if out := string(dmtest.Must(ada.Arc.Command(context.Background(), "message", "outbox").Output())); !strings.Contains(out, "delivered to 1 of 2") {
		t.Errorf("arc message outbox of ada:\n%s\nwant delivered to 1 of 2", out)
	}
	if conv := ada.DM("open", bob.Name+","+cy.Name); !strings.Contains(conv, "you  delivered to 1 of 2\n  hello, both of you\n") || strings.Contains(conv, "just for bob") {
		t.Errorf("ada's group conversation:\n%s", conv)
	}

	// Bob has never seen a message of cy. arc-dm still names cy by its
	// petname, the one that arc whoami prints for cy.
	cyForBob := cy
	got := bob.List()
	want := []dmtest.Block{
		{Header: "  " + ada.Name + "  1 unread", Preview: "just for bob"},
		{Header: "  " + dmtest.GroupName(ada, cyForBob) + "  1 unread", Preview: ada.Name + ": hello, both of you"},
	}
	if len(got) != len(want) {
		t.Fatalf("bob has %d conversations, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if !strings.HasSuffix(got[i].Header, want[i].Header) || got[i].Preview != want[i].Preview {
			t.Errorf("bob's conversation %d is %q, want %q after the time", i, got[i], want[i])
		}
	}

	dmtest.NextSecond()
	bob.DM("send", dmtest.GroupName(ada, cyForBob), "a reply to all")
	ada.Sync()
	cy.Sync()

	if got := cy.List(); len(got) != 1 || !strings.HasSuffix(got[0].Header, dmtest.GroupName(ada, bob)+"  2 unread") {
		t.Errorf("cy's conversations are %q, want one group with 2 unread", got)
	}
	dmtest.AssertInOrder(t, cy.DM("open", ada.Name+","+bob.Name), []string{
		ada.Name + "  new\n  hello, both of you\n",
		bob.Name + "  new\n  a reply to all\n",
	})
	got = ada.List()
	if len(got) != 2 || !strings.HasSuffix(got[0].Header, dmtest.GroupName(bob, cy)+"  1 unread") || got[0].Preview != bob.Name+": a reply to all" {
		t.Errorf("ada's conversations are %q, want the group first, with the reply of bob", got)
	}
}

// TestPeers selects a conversation by the start of a key, and refuses a
// name that selects no conversation or more than one.
func TestPeers(t *testing.T) {
	t.Parallel()
	ada, bob, cy := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "cy")
	ada.DM("send", bob.Key, "from ada")
	cy.DM("send", bob.Key, "from cy")
	bob.Sync()

	if conv := bob.DM("open", ada.Key[:10]); !strings.Contains(conv, "from ada") || strings.Contains(conv, "from cy") {
		t.Errorf("open with the start of ada's key printed:\n%s", conv)
	}
	if out := bob.Refused("open", "nobody"); !strings.Contains(out, `no conversation with "nobody"`) {
		t.Errorf("open nobody said:\n%s", out)
	}
	// Each name starts with the empty text.
	if out := bob.Refused("send", "", "hello"); !strings.Contains(out, "names 2 conversations") {
		t.Errorf("send to a text that names two conversations said:\n%s", out)
	}
}

// TestControlCharacters shows no control character of a sender on the
// terminal: it shows each one escaped, so nothing of the text is lost.
func TestControlCharacters(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "\x1b[2Jcleared\x07\nline two\tend")
	bob.Sync()
	for _, out := range []string{bob.DM("ls"), bob.DM("open", ada.Name)} {
		if strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, "cleared") {
			t.Errorf("arc-dm printed a control character, or lost the text: %q", out)
		}
	}
	if out := bob.DM("open", ada.Name); !strings.Contains(out, "  \\x1b[2Jcleared\\x07\n  line two\tend\n") {
		t.Errorf("open printed %q, want each control character escaped, and the newline and tab kept", out)
	}
}

// TestWatch prints and notifies a message that arrives while the watch
// runs. The watch keeps the message on this machine, and leaves it unread.
func TestWatch(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	notes := filepath.Join(t.TempDir(), "notifications")
	notifier := filepath.Join(t.TempDir(), "notify")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$1\" \"$2\" >> " + notes + "\n"
	if err := os.WriteFile(notifier, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	watch := bob.Command("watch", "-notify", notifier)
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	ada.DM("send", bob.Key, "are you there")
	want := ada.Name + "|are you there\n"
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool {
		data, _ := os.ReadFile(notes)
		return string(data) == want
	}) {
		data, _ := os.ReadFile(notes)
		t.Fatalf("the notifications are %q, want %q\nwatch:\n%s", data, want, stderr.String())
	}
	if !strings.Contains(stdout.String(), ada.Name+"\n  are you there\n") {
		t.Errorf("the watch printed:\n%s", stdout.String())
	}

	// No arc sync: the watch kept the message.
	if got := bob.List(); len(got) != 1 || !strings.HasSuffix(got[0].Header, ada.Name+"  1 unread") {
		t.Errorf("after the watch, bob's conversations are %q, want ada with 1 unread", got)
	}

	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := watch.Wait(); err != nil {
		t.Errorf("the watch ended with %v after an interrupt, want success\n%s", err, stderr.String())
	}
}

// TestHelp lists every command, for each way to ask, and exits with 0.
func TestHelp(t *testing.T) {
	t.Parallel()
	for _, ask := range []string{"help", "-h", "-help", "--help"} {
		out, err := exec.Command(dmtest.Program(), ask).CombinedOutput()
		if err != nil {
			t.Errorf("arc-dm %s failed: %v", ask, err)
		}
		for _, command := range []string{"arc-dm [ls]", "arc-dm open", "arc-dm send", "arc-dm read", "arc-dm watch"} {
			if !strings.Contains(string(out), command) {
				t.Errorf("arc-dm %s does not list %q:\n%s", ask, command, out)
			}
		}
	}
}

// TestAGroupMemberIsAPeer opens and writes to a citizen whom this citizen
// knows only from a group, by the start of its key. With one shared group,
// open opens that group and marks it read; with two, it lists them.
func TestAGroupMemberIsAPeer(t *testing.T) {
	t.Parallel()
	ada, bob, cy := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "cy")
	ada.DM("send", bob.Key+","+cy.Key, "hello, both of you")
	bob.Sync()

	if out := bob.DM("open", cy.Key[:8]); !strings.Contains(out, "hello, both of you") {
		t.Errorf("bob opens cy, with whom he shares one group, by the start of its key:\n%s", out)
	}
	if got := bob.List(); len(got) != 1 || strings.Contains(got[0].Header, "unread") {
		t.Errorf("after open, bob's conversations are %q, want the group read", got)
	}

	dee := dmtest.NewPerson(t, "dee")
	ada.DM("send", bob.Key+","+cy.Key+","+dee.Key, "a second group")
	bob.Sync()
	out := bob.DM("open", cy.Key[:8])
	if !strings.Contains(out, "no direct messages with "+cy.Name) || !strings.Contains(out, "arc-dm open "+dmtest.GroupNames(ada, cy)) || !strings.Contains(out, "arc-dm open "+dmtest.GroupNames(ada, cy, dee)) {
		t.Errorf("bob opens cy, with whom he shares two groups:\n%s", out)
	}

	if out := bob.DM("send", cy.Key[:8], "just for you, cy"); !strings.Contains(out, "queued for "+cy.Name) {
		t.Errorf("bob sends to cy by the start of its key: %s", out)
	}
	cy.Sync()
	if conv := cy.DM("open", bob.Name); !strings.Contains(conv, "just for you, cy") {
		t.Errorf("cy's direct conversation with bob:\n%s", conv)
	}
}

// TestFlagsBeforeTheCommand takes the flags before or after the command.
func TestFlagsBeforeTheCommand(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "flags first")
	bob.Sync()
	flags := []string{"-arc", bob.Arc.Program, "-home", bob.Arc.Home, "-state", bob.State}
	out, err := exec.Command(dmtest.Program(), append(flags, "open", ada.Key[:8])...).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "flags first") {
		t.Fatalf("arc-dm <flags> open: %v\n%s", err, out)
	}
	if got := bob.List(); len(got) != 1 || strings.Contains(got[0].Header, "unread") {
		t.Errorf("the open with flags first used another state: %q", got)
	}
	out, err = exec.Command(dmtest.Program(), append(flags, "-h", "open")...).CombinedOutput()
	if !strings.Contains(string(out), "Usage of arc-dm open") {
		t.Errorf("arc-dm <flags> -h open: %v\n%s", err, out)
	}
}

// TestTheEndOfAPetnameNamesACitizen opens a conversation by the hex digits
// at the end of a petname, which do not start the key.
func TestTheEndOfAPetnameNamesACitizen(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "find me by my suffix")
	bob.Sync()
	suffix := ada.Name[strings.LastIndex(ada.Name, "-")+1:]
	if strings.HasPrefix(ada.Key, suffix) {
		t.Skip("this key starts with the suffix of its petname")
	}
	if conv := bob.DM("open", suffix); !strings.Contains(conv, "find me by my suffix") {
		t.Errorf("bob opens %s by %q:\n%s", ada.Name, suffix, conv)
	}
}

// TestNoArc says how to get arc when it is not on PATH.
func TestNoArc(t *testing.T) {
	t.Parallel()
	command := exec.Command(dmtest.Program(), "ls")
	command.Env = append(os.Environ(), "PATH="+t.TempDir())
	out, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("arc-dm ls with no arc did not fail:\n%s", out)
	}
	if !strings.Contains(string(out), "the arc program is not installed") || !strings.Contains(string(out), "https://republic.sh/join.md") {
		t.Errorf("arc-dm ls with no arc said:\n%s", out)
	}
}

// TestSendText reads standard input only when no text follows the peer,
// refuses a text of only spaces, and stops on a signal while it reads.
func TestSendText(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")

	for _, text := range []string{"", " ", "\t\n"} {
		// Standard input stays open, as in a program that runs arc-dm
		// with a pipe that it does not close.
		command := ada.Command("send", bob.Key, text)
		var out bytes.Buffer
		command.Stdout, command.Stderr = &out, &out
		stdin, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stdin.Close() })
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		if !dmtest.Finish(t, command) {
			t.Errorf("arc-dm send with the text %q waited for standard input", text)
			continue
		}
		if command.ProcessState.Success() || !strings.Contains(out.String(), "a person can see") {
			t.Errorf("arc-dm send with the text %q: %s", text, out.String())
		}
	}

	command := ada.Command("send", bob.Key)
	command.Stdin = strings.NewReader("from standard input\n")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("arc-dm send with the text on standard input: %v\n%s", err, out)
	}
	bob.Sync()
	if out := bob.DM("open", ada.Name); !strings.Contains(out, "  from standard input\n") {
		t.Errorf("bob got %q, want the text of standard input", out)
	}

	command = ada.Command("send", bob.Key)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if !dmtest.Finish(t, command) {
		t.Error("arc-dm send did not stop on SIGTERM while it read standard input")
	}
}
