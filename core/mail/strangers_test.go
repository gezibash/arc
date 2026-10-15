package mail_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"github.com/gezibash/arc/adapters/mailbox"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/internal/testutil"
)

// contactList keeps a contact list in memory, as runtime code keeps it in
// self data.
type contactList struct {
	mu   sync.Mutex
	keys []nostr.PubKey
}

func (l *contactList) Contacts(context.Context) ([]nostr.PubKey, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.keys), nil
}

func (l *contactList) AddContacts(_ context.Context, pks []nostr.PubKey) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, pk := range pks {
		if !slices.Contains(l.keys, pk) {
			l.keys = append(l.keys, pk)
		}
	}
	return nil
}

func (l *contactList) has(pk nostr.PubKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.keys, pk)
}

// filtering turns the filter on for a citizen, with an empty contact list.
// bits sets the work, so that tests that are not about the constant search
// fewer bits. 0 keeps the constant.
func filtering(c citizen, bits int) *contactList {
	list := &contactList{}
	c.mail.Contacts = list
	if bits > 0 {
		mail.SetWork(c.mail, bits)
	}
	return list
}

// ownWraps returns the relay-form wraps that a citizen made for a recipient.
func ownWraps(t *testing.T, c citizen, to nostr.PubKey) []nostr.Event {
	t.Helper()
	return testutil.Must(c.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {to.Hex()}}}))
}

func heldText(t *testing.T, c citizen) []string {
	t.Helper()
	var out []string
	for _, m := range testutil.Must(c.mail.Held(context.Background())) {
		out = append(out, m.Text)
	}
	return out
}

// An older sender makes no work. Its message to a citizen that does not
// have it as a contact is held: not in the inbox, and not acknowledged.
func TestAMessageFromAStrangerWithoutWorkIsHeld(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	filtering(bob, 0)

	alice.send(t, bob, "buy my stuff")
	got := bob.sync(t, r)
	if got.Held != 1 || got.Received != 0 {
		t.Errorf("bob held %d and received %d; want 1 and 0", got.Held, got.Received)
	}
	if inbox := inboxText(bob); len(inbox) != 0 {
		t.Errorf("bob's inbox: %v", inbox)
	}
	if held := heldText(t, bob); !slices.Equal(held, []string{"buy my stuff"}) {
		t.Errorf("bob's held mail: %v", held)
	}
	if back := alice.sync(t, r); back.Delivered != 0 {
		t.Errorf("alice saw %d acknowledgements of held mail", back.Delivered)
	}
}

// A newer sender that the recipient never wrote to makes work of the
// protocol constant, with a real search. The message reaches the inbox,
// and is acknowledged.
func TestAMessageFromAStrangerWithWorkReachesTheInbox(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	filtering(alice, 0)
	filtering(bob, 0)

	alice.send(t, bob, "hello, we met at the market")
	wraps := ownWraps(t, alice, bob.key.Public)
	if len(wraps) != 1 || private.WorkOf(wraps[0]) < 16 {
		t.Fatalf("alice's relay-form wraps: %d, the first with %d bits", len(wraps), private.WorkOf(wraps[0]))
	}
	if got := bob.sync(t, r); got.Received != 1 || got.Held != 0 {
		t.Errorf("bob received %d and held %d; want 1 and 0", got.Received, got.Held)
	}
	if back := alice.sync(t, r); back.Delivered != 1 {
		t.Errorf("alice saw %d acknowledgements, want 1", back.Delivered)
	}
}

// The recipient counts the work of the ID, not the target that the nonce
// tag claims.
func TestAWrapThatClaimsWorkItDoesNotHaveIsHeld(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	filtering(bob, 0)
	ctx := context.Background()

	rumor := private.Rumor(alice.key, mail.MessageKind, "trust me", nostr.Tags{{"p", bob.key.Public.Hex()}}, time.Now())
	seal := testutil.Must(private.Seal(ctx, alice.key, bob.key.Public, rumor))
	once := nostr.Generate()
	conv := testutil.Must(nip44.GenerateConversationKey(bob.key.Public, once))
	wrap := nostr.Event{
		Kind: private.WrapKind, CreatedAt: nostr.Now(), Content: testutil.Must(nip44.Encrypt(seal.String(), conv)),
		Tags: nostr.Tags{{"p", bob.key.Public.Hex()}, {"expiration", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}, {"nonce", "1", "16"}},
	}
	if err := wrap.Sign(once); err != nil {
		t.Fatal(err)
	}
	if private.WorkOf(wrap) >= 16 {
		t.Skip("the ID has 16 bits by chance")
	}
	if err := r.Send(ctx, wrap); err != nil {
		t.Fatal(err)
	}
	if got := bob.sync(t, r); got.Held != 1 || got.Received != 0 {
		t.Errorf("bob held %d and received %d; want 1 and 0", got.Held, got.Received)
	}
}

// To accept a stranger moves its held mail to the inbox, and acknowledges
// it, so the sender's outbox shows it as delivered.
func TestAcceptingAStrangerReleasesItsHeldMail(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	list := filtering(bob, 0)

	alice.send(t, bob, "it is me, your cousin")
	bob.sync(t, r)
	if err := list.AddContacts(context.Background(), []nostr.PubKey{alice.key.Public}); err != nil {
		t.Fatal(err)
	}
	released := testutil.Must(bob.mail.Release(context.Background()))
	if released.Received != 1 {
		t.Errorf("the release moved %d messages, want 1", released.Received)
	}
	if inbox := inboxText(bob); !slices.Equal(inbox, []string{"it is me, your cousin"}) {
		t.Errorf("bob's inbox: %v", inbox)
	}
	if held := heldText(t, bob); len(held) != 0 {
		t.Errorf("bob still holds %v", held)
	}
	alice.sync(t, r)
	if s := states(t, alice)[bob.key.Public]; s != "delivered" {
		t.Errorf("alice's outbox says %s", s)
	}
}

// A contact added on another device releases the held mail at the next
// sync, with no command on this device. The next sync reads an empty
// stick, so the release comes from the held mail, not from the wrap again.
func TestASyncReleasesTheHeldMailOfANewContact(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	list := filtering(bob, 0)
	courier := stick(t)

	alice.send(t, bob, "hello again")
	alice.sync(t, courier)
	bob.sync(t, courier)
	_ = list.AddContacts(context.Background(), []nostr.PubKey{alice.key.Public})
	if got := bob.sync(t, stick(t)); got.Received != 1 {
		t.Errorf("the sync moved %d messages, want 1", got.Received)
	}
	if inbox := inboxText(bob); !slices.Equal(inbox, []string{"hello again"}) {
		t.Errorf("bob's inbox: %v", inbox)
	}
}

// A citizen that sends a message adds the recipient to its contact list.
// The reply of the recipient, without work, reaches the inbox. The first
// message carries work; the reply and the next message carry none.
func TestTheReplyOfARecipientNeedsNoWork(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	aliceList := filtering(alice, 8)
	filtering(bob, 8)

	alice.send(t, bob, "hello bob")
	if !aliceList.has(bob.key.Public) {
		t.Fatal("alice did not add bob to her contacts")
	}
	if w := ownWraps(t, alice, bob.key.Public); len(w) != 1 || w[0].Tags.Find("nonce") == nil {
		t.Fatal("alice's first message has no work")
	}
	bob.sync(t, r)
	bob.send(t, alice, "hello alice")
	if w := ownWraps(t, bob, alice.key.Public); len(w) != 2 || slices.ContainsFunc(w, func(e nostr.Event) bool { return e.Tags.Find("nonce") != nil }) {
		t.Error("bob's reply or acknowledgement has work; alice wrote to him first")
	}
	if got := alice.sync(t, r); got.Received != 1 || got.Held != 0 {
		t.Errorf("alice received %d and held %d; want 1 and 0", got.Received, got.Held)
	}
	alice.send(t, bob, "how are you")
	withWork := 0
	for _, w := range ownWraps(t, alice, bob.key.Public) {
		if w.Tags.Find("nonce") != nil {
			withWork++
		}
	}
	if withWork != 1 {
		t.Errorf("%d of alice's relay-form wraps for bob have work; want only the first message", withWork)
	}
}

// A call request from a stranger reaches the provider without work: the
// gate of the app decides who calls it.
func TestACallFromAStrangerIsNotHeld(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	filtering(bob, 0)
	bob.mail.OnRequest = func(context.Context, nostr.Event) (call.Reply, error) { return call.Reply{Body: "1"}, nil }

	if _, err := alice.mail.Request(context.Background(), bob.key.Public, call.Request{Capability: "counter", Body: "increment"}); err != nil {
		t.Fatal(err)
	}
	if got := bob.sync(t, r); got.Answered != 1 || got.Held != 0 {
		t.Errorf("bob answered %d and held %d; want 1 and 0", got.Answered, got.Held)
	}
}

// The author of a group message decides the filter. A contact among its p
// tags does not admit it.
func TestAGroupMessageFromAStrangerIsHeld(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob, carol := newCitizen(t, r), newCitizen(t, r), newCitizen(t, r)
	_ = filtering(bob, 0).AddContacts(context.Background(), []nostr.PubKey{carol.key.Public})

	alice.sendGroup(t, "join my scheme", bob, carol)
	if got := bob.sync(t, r); got.Held != 1 || got.Received != 0 {
		t.Errorf("bob held %d and received %d; want 1 and 0", got.Held, got.Received)
	}
}

// The held bucket keeps at most MaxHeld seals. The next one pushes out the
// oldest. The test fills a cap of 3, not of 200, to keep it fast.
func TestTheHeldMailHasALimit(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	filtering(bob, 0)
	const limit = 3
	mail.SetMaxHeld(bob.mail, limit)
	courier := stick(t)
	clock := time.Now()
	mail.SetNow(bob.mail, func() time.Time { return clock })

	alice.send(t, bob, "the first")
	alice.sync(t, courier)
	bob.sync(t, courier)
	for i := range limit {
		clock = clock.Add(time.Second)
		alice.send(t, bob, "message "+strconv.Itoa(i))
	}
	alice.sync(t, courier)
	bob.sync(t, courier)
	// The stick still holds each wrap. A second sync must not bring the
	// dropped seal back.
	bob.sync(t, courier)
	held := heldText(t, bob)
	if len(held) != limit || slices.Contains(held, "the first") {
		t.Errorf("bob holds %d messages; the first is still there: %v", len(held), slices.Contains(held, "the first"))
	}
}

// Held mail goes when its wrap expires.
func TestHeldMailExpiresWithItsWrap(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	filtering(bob, 0)

	alice.send(t, bob, "a week from now this is gone")
	bob.sync(t, r)
	mail.SetNow(bob.mail, func() time.Time { return time.Now().Add(private.MaxAge + time.Hour) })
	bob.sync(t, stick(t))
	if held := heldText(t, bob); len(held) != 0 {
		t.Errorf("bob holds %v after the wrap expired", held)
	}
	if n := len(testutil.Must(bob.mail.Held(context.Background()))); n != 0 {
		t.Errorf("Held lists %d", n)
	}
}

// A stranger's message crosses a stick, with no relay: the courier form
// carries the work too.
func TestAMessageFromAStrangerCrossesAStick(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	filtering(alice, 8)
	filtering(bob, 8)
	courier := stick(t)

	alice.send(t, bob, "by hand")
	alice.sync(t, courier)
	if got := bob.sync(t, courier); got.Received != 1 || got.Held != 0 {
		t.Errorf("bob received %d and held %d; want 1 and 0", got.Received, got.Held)
	}
}

// A held seal moves to the inbox when another wrap of it arrives with
// work: here a courier form without work, then the relay form with work.
func TestAHeldSealMovesWhenAWrapWithWorkArrives(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	filtering(bob, 8)
	courier := stick(t)
	ctx := context.Background()

	rumor := private.Rumor(alice.key, mail.MessageKind, "twice", nostr.Tags{{"p", bob.key.Public.Hex()}}, time.Now())
	seal := testutil.Must(private.Seal(ctx, alice.key, bob.key.Public, rumor))
	expires := time.Now().Add(time.Hour)
	bare := testutil.Must(private.WrapSeal(seal, bob.key.Public, private.CourierForm, private.WrapKind, expires))
	worked := testutil.Must(private.WrapSealWork(ctx, seal, bob.key.Public, private.RelayForm, private.WrapKind, expires, 8))
	if err := courier.Send(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := r.Send(ctx, worked); err != nil {
		t.Fatal(err)
	}
	if got := bob.sync(t, courier); got.Held != 1 {
		t.Fatalf("bob held %d wraps, want 1", got.Held)
	}
	if got := bob.sync(t, r); got.Received != 1 {
		t.Errorf("the wrap with work moved %d messages, want 1", got.Received)
	}
	if held := heldText(t, bob); len(held) != 0 {
		t.Errorf("bob still holds %v", held)
	}
	if inbox := inboxText(bob); !slices.Equal(inbox, []string{"twice"}) {
		t.Errorf("bob's inbox: %v", inbox)
	}
}

// Mail from before the filter has no record of who wrote. The first send
// finds the seal of the recipient's older message, and makes no work.
func TestAnOlderMessageFromTheRecipientCountsOnce(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	bob.send(t, alice, "from before the filter")
	alice.sync(t, r)

	filtering(alice, 8)
	alice.send(t, bob, "and after it")
	if slices.ContainsFunc(ownWraps(t, alice, bob.key.Public), func(e nostr.Event) bool { return e.Tags.Find("nonce") != nil }) {
		t.Error("alice made work for a recipient that wrote to her before the filter")
	}
}

// An update must not make each correspondent a stranger. The first sync
// with a contact list puts each recipient of the outbox on it, once: the
// mail of arc v0.31.0 in testdata/outbox-v0.31.0 wrote to bob and carol. A
// key that the citizen removes after that stays off.
func TestAnOldOutboxSeedsTheContacts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"events.db", "mail.db"} {
		unzip(t, filepath.Join("testdata", "outbox-v0.31.0", name+".gz"), filepath.Join(dir, name))
	}
	var secrets map[string]string
	if err := json.Unmarshal(testutil.Must(os.ReadFile(filepath.Join("testdata", "outbox-v0.31.0", "keys.json"))), &secrets); err != nil {
		t.Fatal(err)
	}
	aliceKey, bobKey, carolKey := mustKey(t, secrets["alice"]), mustKey(t, secrets["bob"]), mustKey(t, secrets["carol"])
	s, err := boltstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	m, err := mailbox.Open(dir, aliceKey, &node.Node{Store: s}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	list := &contactList{}
	m.Contacts = list

	alice := citizen{dir: dir, key: aliceKey, mail: m}
	alice.sync(t, stick(t))
	if !list.has(bobKey.Public) || !list.has(carolKey.Public) {
		t.Fatalf("the contacts after the first sync: %v", list.keys)
	}
	list.mu.Lock()
	list.keys = nil
	list.mu.Unlock()
	alice.sync(t, stick(t))
	if len(list.keys) != 0 {
		t.Errorf("a second sync seeded the contacts again: %v", list.keys)
	}
}
