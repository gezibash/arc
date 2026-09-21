package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	alice = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	bob   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	carol = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
)

func testDM(t *testing.T) *server {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mailboxes"), 0o700); err != nil {
		t.Fatal(err)
	}

	return &server{
		config: &config{
			Root: root, MailboxBudget: 512 * 1024 * 1024, RetractWindow: 600,
			MaxAttach: 6 * 1024 * 1024, MaxBody: 98_304,
		},
		store: &store{root: root},
	}
}

// token is a stand-in for a sealed body. The provider never opens one.
func token(text string) string {
	return "sealed-v1:" + base64.StdEncoding.EncodeToString([]byte("fake|"+text))
}

// sendBody holds one token for the recipient and one for the sender.
func sendBody(text string) string {
	return token(text+"@peer") + "\n" + token(text+"@self") + "\n"
}

// groupBody holds one token for each recipient, then one for the sender.
func groupBody(text string, count int) string {
	var body strings.Builder
	for index := 1; index <= count; index++ {
		body.WriteString(token(fmt.Sprintf("%s@%d", text, index)) + "\n")
	}
	body.WriteString(token(text+"@self") + "\n")
	return body.String()
}

func run(t *testing.T, s *server, from, message string) *answer {
	t.Helper()

	got, err := s.run(from, message)
	if err != nil {
		t.Fatalf("%q: %v", message, err)
	}
	return got
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

func sendOne(t *testing.T, s *server, from, to, text string, extra ...string) string {
	t.Helper()

	line := "send " + to + " " + strings.Join(extra, " ")
	got := run(t, s, from, line+"\n"+sendBody(text))

	id, found := strings.CutPrefix(got.reply, "id: ")
	if !found {
		t.Fatalf("send gave %q", got.reply)
	}
	return id
}

func rows(reply string) []string { return strings.Split(reply, "\n") }

func fields(row string) []string { return strings.Split(row, "\t") }

func TestULIDIsTwentySixCharactersAndRises(t *testing.T) {
	first, err := newULID()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := newULID()

	if len(first) != 26 || !validULID(first) {
		t.Errorf("id = %q", first)
	}
	if first >= second {
		t.Errorf("%q does not stand before %q", first, second)
	}
	if validULID("nope") {
		t.Error("a name that is not an id passed")
	}
}

func TestTheCallerNeedsAKey(t *testing.T) {
	s := testDM(t)

	fails(t, s, "", "whoami", "forbidden")
	fails(t, s, "abc", "whoami", "forbidden")

	if got := run(t, s, alice, "whoami"); got.reply != alice {
		t.Errorf("whoami = %q", got.reply)
	}
}

func TestSendStoresOneSealedCopyForEachMailbox(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "hi")

	forBob, err := s.store.getMessage(bob, id)
	if err != nil {
		t.Fatal(err)
	}
	forAlice, err := s.store.getMessage(alice, id)
	if err != nil {
		t.Fatal(err)
	}

	if forBob.Body != token("hi@peer") || forAlice.Body != token("hi@self") {
		t.Errorf("the bodies are %q and %q", forBob.Body, forAlice.Body)
	}
	if forBob.From != alice || len(forBob.To) != 1 || forBob.To[0] != bob || forBob.Enc != "sealed-v1" {
		t.Errorf("message = %+v", forBob)
	}

	if got := s.store.receiptsOf(bob, id); len(got) != 1 || got[0].Event != "delivered" {
		t.Errorf("the receipts of bob are %+v", got)
	}
	if got := s.store.receiptsOf(alice, id); len(got) != 0 {
		t.Errorf("the sender holds receipts: %+v", got)
	}
}

// The provider holds ciphertext only.
func TestNoPlainTextReachesTheDisk(t *testing.T) {
	s := testDM(t)
	sendOne(t, s, alice, bob, "the secret words")

	found, err := exec.Command("grep", "-rl", "secret words", s.store.root).CombinedOutput()
	if err == nil {
		t.Errorf("plain text lies in %s", found)
	}
}

func TestSendRefusesABodyThatIsNotSealed(t *testing.T) {
	s := testDM(t)

	fails(t, s, alice, "send "+bob+"\nplain text\nmore", "unsealed")
	fails(t, s, alice, "send "+bob+"\n"+token("x"), "unsealed")
	fails(t, s, alice, "send "+bob, "unsealed")

	big := "sealed-v1:" + strings.Repeat("A", s.config.MaxBody)
	fails(t, s, alice, "send "+bob+"\n"+big+"\n"+big, "too_large")

	fails(t, s, alice, "send bob\n"+sendBody("x"), "invalid_address")
}

func TestSendCarriesTheReplyTo(t *testing.T) {
	s := testDM(t)

	first := sendOne(t, s, alice, bob, "q")
	second := sendOne(t, s, bob, alice, "a", `--reply-to "`+first+`"`)

	got := run(t, s, alice, "read "+second)
	if !strings.Contains(got.reply, "reply_to: "+first) {
		t.Errorf("read = %q", got.reply)
	}
}

func TestInboxListsBothDirections(t *testing.T) {
	s := testDM(t)
	first := sendOne(t, s, alice, bob, "one")
	second := sendOne(t, s, bob, alice, "two")

	got := rows(run(t, s, bob, "inbox").reply)
	if len(got) != 2 {
		t.Fatalf("inbox holds %d rows", len(got))
	}
	if !strings.HasPrefix(got[0], first+"\tin\t"+alice+"\t") {
		t.Errorf("the first row is %q", got[0])
	}
	if !strings.HasPrefix(got[1], second+"\tout\t"+alice+"\t") {
		t.Errorf("the second row is %q", got[1])
	}

	unread := rows(run(t, s, bob, "inbox --unread").reply)
	if len(unread) != 1 || !strings.HasPrefix(unread[0], first) {
		t.Errorf("the unread rows are %v", unread)
	}
}

func TestInboxTakesSinceAndLimit(t *testing.T) {
	s := testDM(t)
	first := sendOne(t, s, alice, bob, "one")
	second := sendOne(t, s, alice, bob, "two")
	third := sendOne(t, s, alice, bob, "three")

	got := rows(run(t, s, bob, "inbox --since "+first).reply)
	if len(got) != 2 || !strings.HasPrefix(got[0], second) {
		t.Errorf("since gave %v", got)
	}

	got = rows(run(t, s, bob, "inbox --limit 1").reply)
	if len(got) != 1 || !strings.HasPrefix(got[0], third) {
		t.Errorf("limit gave %v", got)
	}
}

func TestReadWritesOneReadReceipt(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "hi")

	got := run(t, s, bob, "read "+id)
	if !strings.Contains(got.reply, "body: "+token("hi@peer")) {
		t.Errorf("read = %q", got.reply)
	}

	run(t, s, bob, "read "+id)

	reads := 0
	for _, one := range s.store.receiptsOf(bob, id) {
		if one.Event == "read" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the mailbox holds %d read receipts", reads)
	}

	fails(t, s, carol, "read "+id, "not_found")
	fails(t, s, bob, "read nonsense", "not_found")
}

func TestStatusShowsTheReceiptsToTheSenderOnly(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "hi")
	run(t, s, bob, "read "+id)

	got := rows(run(t, s, alice, "status "+id).reply)
	if len(got) != 2 || !strings.HasPrefix(got[0], "delivered ") || !strings.HasPrefix(got[1], "read ") {
		t.Errorf("status = %v", got)
	}

	fails(t, s, bob, "status "+id, "forbidden")
}

func TestAckRefusesAMessageThatWentOut(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "hi")

	fails(t, s, alice, "ack "+id, "forbidden")
	if got := run(t, s, bob, "ack "+id); got.reply != "acked 1" {
		t.Errorf("ack = %q", got.reply)
	}
}

func TestArchiveHidesFromTheInboxAndNotFromTheThread(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "hi")

	run(t, s, bob, "archive "+id)
	if got := run(t, s, bob, "inbox"); got.reply != "no messages" {
		t.Errorf("inbox = %q", got.reply)
	}
	if got := run(t, s, bob, "thread "+alice); !strings.HasPrefix(got.reply, id) {
		t.Errorf("thread = %q", got.reply)
	}
}

func TestThreadWithBodiesMarksTheMessagesThatArrived(t *testing.T) {
	s := testDM(t)
	first := sendOne(t, s, alice, bob, "q")
	second := sendOne(t, s, bob, alice, "a", `--reply-to "`+first+`"`)

	got := rows(run(t, s, bob, "thread "+alice+` --bodies "true"`).reply)
	if len(got) != 3 {
		t.Fatalf("the thread holds %d rows: %v", len(got), got)
	}
	if got[0] != alice+" · 2 messages, 1 unread" {
		t.Errorf("the header is %q", got[0])
	}

	first_row := fields(got[1])
	if first_row[0] != first || first_row[1] != "in" || first_row[4] != "-" || first_row[5] != "unread" {
		t.Errorf("the first row is %v", first_row)
	}
	if first_row[6] != token("q@peer") {
		t.Errorf("the body is %q", first_row[6])
	}

	second_row := fields(got[2])
	if second_row[0] != second || second_row[1] != "out" || second_row[4] != first || second_row[5] != "delivered" {
		t.Errorf("the second row is %v", second_row)
	}

	// The first call marked the message read.
	got = rows(run(t, s, bob, "thread "+alice+` --bodies "true"`).reply)
	if got[0] != alice+" · 2 messages, 0 unread" {
		t.Errorf("the header is %q", got[0])
	}
	if fields(got[1])[5] != "read" {
		t.Errorf("the state is %q", fields(got[1])[5])
	}

	// Alice sees that on the copy that went out.
	got = rows(run(t, s, alice, "thread "+bob+` --bodies "true"`).reply)
	if fields(got[1])[5] != "read" {
		t.Errorf("the state of the sender is %q", fields(got[1])[5])
	}
}

func TestThreadHidesReadWhenThePeerTurnedReceiptsOff(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "q")

	run(t, s, bob, "settings receipts off")
	run(t, s, bob, "read "+id)

	got := rows(run(t, s, alice, "thread "+bob+` --bodies "true"`).reply)
	if fields(got[1])[5] != "delivered" {
		t.Errorf("the state is %q", fields(got[1])[5])
	}
}

func TestReactKeepsTheLatestReactionOfEachCitizen(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "hi")

	got, err := s.run(bob, "react "+id+" 👍")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.reply, "reacted 👍 ") {
		t.Errorf("react = %q", got.reply)
	}
	if len(got.events) != 1 || got.events[0].To != alice || got.events[0].Topic != "dm.reaction" {
		t.Fatalf("the events are %+v", got.events)
	}
	if got.events[0].Meta["from"] != bob || got.events[0].Meta["to"] != bob || got.events[0].Meta["value"] != "👍" {
		t.Errorf("the meta is %v", got.events[0].Meta)
	}

	run(t, s, bob, "react "+id+" ❤️")
	run(t, s, alice, "react "+id+" 🎉")
	fails(t, s, bob, "react "+id+" toolong", "invalid_reaction")
	fails(t, s, carol, "react "+id+" 👍", "not_found")

	thread := rows(run(t, s, alice, "thread "+bob+` --bodies "true"`).reply)
	want := "delivered;reaction=🎉:" + alice + ",❤️:" + bob
	if flags := fields(thread[1])[5]; flags != want {
		t.Errorf("flags = %q, want %q", flags, want)
	}

	read := run(t, s, bob, "read "+id).reply
	if !strings.Contains(read, "reactions: 🎉 "+alice+" ❤️ "+bob) {
		t.Errorf("read = %q", read)
	}

	cleared := run(t, s, bob, "react "+id+" none")
	if !strings.HasPrefix(cleared.reply, "cleared ") {
		t.Errorf("clear = %q", cleared.reply)
	}
	if read := run(t, s, bob, "read "+id).reply; !strings.Contains(read, "reactions: 🎉 "+alice+"\n") {
		t.Errorf("read = %q", read)
	}
}

func TestRetractEmptiesTheCopyOfTheRecipient(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "oops")

	fails(t, s, bob, "retract "+id, "forbidden")

	got, err := s.run(alice, "retract "+id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.events) != 1 || got.events[0].To != bob || got.events[0].Topic != "dm.retracted" {
		t.Fatalf("the events are %+v", got.events)
	}

	forBob, _ := s.store.getMessage(bob, id)
	forAlice, _ := s.store.getMessage(alice, id)
	if forBob.Body != "" || forAlice.Body != token("oops@self") {
		t.Errorf("the bodies are %q and %q", forBob.Body, forAlice.Body)
	}

	if read := run(t, s, bob, "read "+id).reply; !strings.HasSuffix(read, "retracted: yes\nbody: ") {
		t.Errorf("read = %q", read)
	}

	thread := rows(run(t, s, bob, "thread "+alice+` --bodies "true"`).reply)
	row := fields(thread[1])
	if row[5] != "retracted" || row[6] != "" {
		t.Errorf("the row is %v", row)
	}
}

func TestRetractFailsAfterTheWindow(t *testing.T) {
	s := testDM(t)
	s.config.RetractWindow = 0

	id := sendOne(t, s, alice, bob, "late")

	// The timestamp of a message holds whole seconds, so the window closes
	// once the clock passes the second of the send.
	time.Sleep(1100 * time.Millisecond)
	fails(t, s, alice, "retract "+id, "too_late")
}

func TestBlockRefusesASendAndUnblockRestoresIt(t *testing.T) {
	s := testDM(t)

	run(t, s, bob, "block "+alice)
	if got := run(t, s, bob, "blocked"); got.reply != alice {
		t.Errorf("blocked = %q", got.reply)
	}
	fails(t, s, alice, "send "+bob+"\n"+sendBody("x"), "blocked")

	run(t, s, bob, "unblock "+alice)
	sendOne(t, s, alice, bob, "again")

	if got := run(t, s, bob, "blocked"); got.reply != "no blocked keys" {
		t.Errorf("blocked = %q", got.reply)
	}
}

func TestSendToYourselfStoresOneCopy(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, alice, "note")

	if _, err := s.store.getMessage(alice, id); err != nil {
		t.Fatal(err)
	}

	conversations := rows(run(t, s, alice, "conversations").reply)
	if len(conversations) != 2 || !strings.HasPrefix(conversations[1], alice+"\t") {
		t.Errorf("conversations = %v", conversations)
	}
}

func TestConversationsCountsTheUnreadOfEachPeer(t *testing.T) {
	s := testDM(t)
	sendOne(t, s, alice, bob, "one")
	sendOne(t, s, carol, bob, "two")
	sendOne(t, s, alice, bob, "three")

	got := rows(run(t, s, bob, "conversations").reply)
	if got[0] != "3 unread in 2 conversations" {
		t.Errorf("the header is %q", got[0])
	}
	if !strings.HasPrefix(got[1], alice+"\t") {
		t.Errorf("the newest conversation is %q", got[1])
	}
	if fields(got[1])[3] != "2" || fields(got[2])[3] != "1" {
		t.Errorf("the counts are %v and %v", fields(got[1]), fields(got[2]))
	}
}

func TestMuteStopsTheUnreadCountAndKeepsDelivery(t *testing.T) {
	s := testDM(t)
	sendOne(t, s, alice, bob, "one")

	run(t, s, bob, "mute "+alice)
	sendOne(t, s, alice, bob, "two")

	got := rows(run(t, s, bob, "conversations").reply)
	if got[0] != "0 unread in 1 conversations" {
		t.Errorf("the header is %q", got[0])
	}
	if fields(got[1])[4] != "muted" {
		t.Errorf("the row is %v", fields(got[1]))
	}
	if inbox := run(t, s, bob, "inbox --unread"); inbox.reply != "no messages" {
		t.Errorf("the unread inbox is %q", inbox.reply)
	}
	// The messages still arrived.
	if inbox := rows(run(t, s, bob, "inbox").reply); len(inbox) != 2 {
		t.Errorf("the inbox holds %d rows", len(inbox))
	}

	run(t, s, bob, "unmute "+alice)
	if got := run(t, s, bob, "muted"); got.reply != "no muted keys" {
		t.Errorf("muted = %q", got.reply)
	}
}

func TestSendToAGroupGivesEachRecipientItsOwnToken(t *testing.T) {
	s := testDM(t)

	got, err := s.run(alice, "send --to "+bob+","+carol+"\n"+groupBody("hey", 2))
	if err != nil {
		t.Fatal(err)
	}

	id := strings.TrimPrefix(got.reply, "id: ")
	if len(got.events) != 2 {
		t.Fatalf("the events are %+v", got.events)
	}
	if got.events[0].To != bob || got.events[0].Body != token("hey@1") {
		t.Errorf("the first event is %+v", got.events[0])
	}
	if got.events[1].To != carol || got.events[1].Body != token("hey@2") {
		t.Errorf("the second event is %+v", got.events[1])
	}

	forBob, _ := s.store.getMessage(bob, id)
	forCarol, _ := s.store.getMessage(carol, id)
	forAlice, _ := s.store.getMessage(alice, id)

	if forBob.Body != token("hey@1") || forCarol.Body != token("hey@2") || forAlice.Body != token("hey@self") {
		t.Errorf("the bodies are %q, %q and %q", forBob.Body, forCarol.Body, forAlice.Body)
	}
	if len(forBob.To) != 2 {
		t.Errorf("the recipients are %v", forBob.To)
	}

	// A group is its own conversation, keyed on every other participant in
	// any order. It does not reach the threads of one to one.
	for _, key := range []string{alice + "," + carol, carol + "," + alice} {
		if got := run(t, s, bob, "thread "+key); got.reply == "no messages" {
			t.Errorf("the group thread %s holds nothing", key)
		}
	}
	for _, key := range []string{alice, carol} {
		if got := run(t, s, bob, "thread "+key); got.reply != "no messages" {
			t.Errorf("the thread of %s holds the group message", key)
		}
	}
	fails(t, s, bob, "thread "+alice+",nope", "invalid_address")

	conversations := rows(run(t, s, bob, "conversations").reply)
	if !strings.HasPrefix(conversations[1], alice+","+carol+"\t") {
		t.Errorf("the conversation key is %q", conversations[1])
	}

	run(t, s, carol, "read "+id)
	status := rows(run(t, s, alice, "status "+id).reply)
	if len(status) != 3 || !strings.HasPrefix(status[2], "read ") || !strings.Contains(status[2], carol) {
		t.Errorf("status = %v", status)
	}
}

func TestSendToAGroupNeedsOneTokenForEachHolder(t *testing.T) {
	s := testDM(t)

	fails(t, s, alice, "send --to "+bob+","+carol+"\n"+sendBody("x"), "unsealed body needs 3")
	fails(t, s, alice, "send --to nope\n"+sendBody("x"), "invalid_address")
}

func TestSendToAGroupFailsWhenOneRecipientBlocksTheSender(t *testing.T) {
	s := testDM(t)
	run(t, s, carol, "block "+alice)

	fails(t, s, alice, "send --to "+bob+","+carol+"\n"+groupBody("x", 2), "blocked "+carol)
}

func TestAttachmentsAreStoredForEachHolder(t *testing.T) {
	s := testDM(t)

	body := strings.Join([]string{
		token("hi@peer"), token("hi@self"),
		"attach:plan.md:" + token("file@peer"),
		"attach:plan.md:" + token("file@self"),
	}, "\n")

	got := run(t, s, alice, "send "+bob+"\n"+body)
	id := strings.TrimPrefix(got.reply, "id: ")

	if fetched := run(t, s, bob, "fetch "+id+" plan.md"); fetched.reply != token("file@peer") {
		t.Errorf("the attachment of bob is %q", fetched.reply)
	}
	if fetched := run(t, s, alice, "fetch "+id+" plan.md"); fetched.reply != token("file@self") {
		t.Errorf("the attachment of alice is %q", fetched.reply)
	}

	read := run(t, s, bob, "read "+id).reply
	if !strings.Contains(read, "attachments: plan.md (") {
		t.Errorf("read = %q", read)
	}

	fails(t, s, carol, "fetch "+id+" plan.md", "not_found")
	fails(t, s, bob, "fetch "+id+" missing.md", "not_found")
}

// A name that climbs out of the directory of the message never writes.
func TestAnAttachmentNameNeverClimbs(t *testing.T) {
	s := testDM(t)

	for _, name := range []string{"../escape", ".hidden", "a/b"} {
		body := strings.Join([]string{
			token("hi@peer"), token("hi@self"),
			"attach:" + name + ":" + token("file@peer"),
			"attach:" + name + ":" + token("file@self"),
		}, "\n")

		if _, err := s.run(alice, "send "+bob+"\n"+body); err == nil {
			t.Errorf("the name %q passed", name)
		}
	}

	id := sendOne(t, s, alice, bob, "hi")
	for _, name := range []string{"../../etc/passwd", ".", ".."} {
		fails(t, s, bob, "fetch "+id+" "+name, "not_found")
	}
}

func TestTheQuotaHoldsASendOverTheBudget(t *testing.T) {
	s := testDM(t)
	sendOne(t, s, alice, bob, "hi")

	if got := run(t, s, bob, "quota"); !strings.HasPrefix(got.reply, "used ") {
		t.Errorf("quota = %q", got.reply)
	}

	s.config.MailboxBudget = 10
	fails(t, s, alice, "send "+bob+"\n"+sendBody("x"), "too_large")
}

func TestPurgeTakesOnlyTheOlderMessagesOfTheCaller(t *testing.T) {
	s := testDM(t)
	first := sendOne(t, s, alice, bob, "one")
	second := sendOne(t, s, alice, bob, "two")

	if got := run(t, s, bob, "purge --before "+second); got.reply != "purged 1" {
		t.Errorf("purge = %q", got.reply)
	}
	if _, err := s.store.getMessage(bob, first); err == nil {
		t.Error("the old message is still there")
	}
	if _, err := s.store.getMessage(bob, second); err != nil {
		t.Error("the new message went")
	}
	// The copy of the sender stays.
	if _, err := s.store.getMessage(alice, first); err != nil {
		t.Error("the purge of one mailbox took the copy of another")
	}

	fails(t, s, bob, "purge", "missing --before")
	fails(t, s, bob, "purge --before nonsense", "not_found")
}

func TestAMessageOfTheFirstReleaseStillReads(t *testing.T) {
	s := testDM(t)
	id := sendOne(t, s, alice, bob, "old")

	// The first release named one recipient as a string.
	path := s.store.msgPath(alice, id)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), `"to":["`+bob+`"]`, `"to":"`+bob+`"`, 1)
	if changed == string(data) {
		t.Fatalf("the message does not hold a list: %s", data)
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}

	thread := rows(run(t, s, alice, "thread "+bob+` --bodies "true"`).reply)
	if row := fields(thread[1]); row[0] != id || row[1] != "out" || row[2] != bob {
		t.Errorf("the row is %v", row)
	}
	run(t, s, alice, "conversations")
	run(t, s, alice, "status "+id)
}

func TestTheUsageCounterFollowsEveryWrite(t *testing.T) {
	s := testDM(t)
	sendOne(t, s, alice, bob, "hi")

	used := s.store.usage(bob)
	if used <= 0 {
		t.Fatalf("the mailbox holds %d bytes", used)
	}

	// A counter that is broken is rebuilt from the mailbox.
	os.WriteFile(s.store.usagePath(bob), []byte("nonsense"), 0o600)
	if rebuilt := s.store.usage(bob); rebuilt != used {
		t.Errorf("the counter rebuilt to %d, want %d", rebuilt, used)
	}

	// A counter below zero is rebuilt as well.
	s.store.bumpUsage(bob, -1_000_000)
	if rebuilt := s.store.usage(bob); rebuilt != used {
		t.Errorf("the counter after a bad move is %d, want %d", rebuilt, used)
	}

	run(t, s, bob, "purge --before "+strings.Repeat("Z", 26))
	if after := s.store.usage(bob); after != 0 {
		t.Errorf("the mailbox holds %d bytes after the purge", after)
	}
}

func TestSettings(t *testing.T) {
	s := testDM(t)

	if got := run(t, s, alice, "settings"); got.reply != "receipts on" {
		t.Errorf("settings = %q", got.reply)
	}
	if got := run(t, s, alice, "settings receipts off"); got.reply != "receipts off" {
		t.Errorf("settings = %q", got.reply)
	}
	if got := run(t, s, alice, "settings"); got.reply != "receipts off" {
		t.Errorf("settings = %q", got.reply)
	}

	fails(t, s, alice, "settings colour blue", "invalid_setting")
	fails(t, s, alice, "settings receipts maybe", "invalid_setting")
}

func TestUnknownCommandAndHelp(t *testing.T) {
	s := testDM(t)

	fails(t, s, alice, "nonsense", "unknown_command")
	if got := run(t, s, alice, "help"); !strings.HasPrefix(got.reply, "dm commands") {
		t.Errorf("help = %q", got.reply)
	}
	if got := run(t, s, alice, ""); !strings.HasPrefix(got.reply, "dm commands") {
		t.Errorf("the empty command gave %q", got.reply)
	}
}
