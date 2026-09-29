package mail

import (
	"context"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/private"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
)

type observedTransport struct{ beforeSend func() }

type deniedDecrypt struct {
	keys.Key
	err error
}

func (d deniedDecrypt) Decrypt(context.Context, string, nostr.PubKey) (string, error) {
	return "", d.err
}

func TestRecordedMailReadFailureIsNotAnEmptyResult(t *testing.T) {
	m, _, key, _ := openMail(t, nil)
	if _, err := m.Send(context.Background(), keys.Generate().Public, "recorded message"); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("the signer refuses decryption")
	m.key = deniedDecrypt{Key: key, err: denied}
	if _, err := m.Rumors(context.Background(), []nostr.Kind{MessageKind}); !errors.Is(err, denied) {
		t.Fatalf("inbox read=%v", err)
	}
	if _, err := m.Outbox(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("outbox read=%v", err)
	}
}

func (*observedTransport) Name() string { return "observer" }
func (o *observedTransport) Send(context.Context, nostr.Event) error {
	o.beforeSend()
	return errors.New("offline")
}
func (*observedTransport) Fetch(context.Context, nostr.Filter) (transport.Batch, error) {
	return transport.Batch{}, nil
}

func openMail(t *testing.T, via []transport.Transport) (*Mail, string, keys.Key, *node.Node) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	key := keys.Generate()
	n := &node.Node{Store: s}
	m, err := Open(dir, key, n, via)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, dir, key, n
}
func TestOutgoingStatePrecedesTransmission(t *testing.T) {
	via := &observedTransport{}
	m, _, _, _ := openMail(t, []transport.Transport{via})
	sent := 0
	via.beforeSend = func() {
		sent++
		out, err := m.Outbox(context.Background())
		if err != nil || len(out) != 1 || out[0].State(time.Now()) != "pending" {
			t.Fatalf("transmission before durable outbox: %+v %v", out, err)
		}
	}
	if _, err := m.Request(context.Background(), keys.Generate().Public, call.Request{Body: "increment"}); err != nil {
		t.Fatal(err)
	}
	if sent != 2 {
		t.Fatalf("sends=%d, want two encrypted forms", sent)
	}
	m.Close()
	if _, err := m.Request(context.Background(), keys.Generate().Public, call.Request{Body: "increment"}); err == nil {
		t.Fatal("closed mail database accepted a request")
	}
	if sent != 2 {
		t.Fatal("sent a request whose outbox commit failed")
	}
}
func TestInterruptedExecutionIsNotRepeated(t *testing.T) {
	m, dir, key, n := openMail(t, nil)
	sender := keys.Generate()
	request := call.RequestRumor(sender, key.Public, call.Request{Body: "increment"}, time.Now())
	seal, err := private.Seal(context.Background(), sender, key.Public, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.Store.Save(seal); err != nil {
		t.Fatal(err)
	}
	if err = m.put(inboxBucket, request.ID.Hex(), incoming{Seal: seal.ID.Hex(), State: "processing", Started: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m.Close()
	m, err = Open(dir, key, n, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	called := false
	m.OnRequest = func(context.Context, nostr.Event) (call.Reply, error) {
		called = true
		return call.Reply{Body: "2"}, nil
	}
	report, err := m.Sync(context.Background(), file.Dir{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if called || report.Answered != 1 {
		t.Fatalf("called=%v answered=%d", called, report.Answered)
	}
	var entry incoming
	if _, err = m.get(inboxBucket, request.ID.Hex(), &entry); err != nil {
		t.Fatal(err)
	}
	sealed, err := m.savedSeal(entry.ReplySeal)
	if err != nil {
		t.Fatal(err)
	}
	rumor, err := private.OpenOwnSeal(context.Background(), key, sealed, sender.Public)
	if err != nil {
		t.Fatal(err)
	}
	_, reply := call.ReadReply(rumor)
	if entry.State != "completed" || reply.Err != "outcome_unknown a previous execution did not record a result" {
		t.Fatalf("entry=%+v reply=%+v", entry, reply)
	}
}
