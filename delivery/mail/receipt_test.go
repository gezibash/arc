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
	"github.com/gezibash/arc/delivery/transport/file"
)

func requestWrap(t *testing.T, recipient nostr.PubKey) (nostr.Event, nostr.Event, nostr.Event) {
	t.Helper()
	sender := keys.Generate()
	rumor := call.RequestRumor(sender, recipient, call.Request{Body: "increment"}, time.Now())
	seal, err := private.Seal(context.Background(), sender, recipient, rumor)
	if err != nil {
		t.Fatal(err)
	}
	wrap, err := private.WrapSeal(seal, recipient, private.CourierForm, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rumor, seal, wrap
}

type interruptedReceiptStore struct {
	node.EventStore
	before func() error
	after  func()
}

func (s interruptedReceiptStore) Save(event nostr.Event) (store.Result, error) {
	if event.Kind == private.SealKind && s.before != nil {
		if err := s.before(); err != nil {
			return store.Result{}, err
		}
	}
	result, err := s.EventStore.Save(event)
	if err == nil && event.Kind == private.SealKind && s.after != nil {
		s.after()
	}
	return result, err
}

func TestReceiptRecoversAcrossStoreInterruptions(t *testing.T) {
	for _, stage := range []string{"before_seal", "after_seal"} {
		t.Run(stage, func(t *testing.T) {
			m, dir, key, n := openMail(t, nil)
			_, _, wrap := requestWrap(t, key.Public)
			original := n.Store
			broken := interruptedReceiptStore{EventStore: original}
			if stage == "before_seal" {
				broken.before = func() error { return errors.New("event store temporarily unavailable") }
			} else {
				broken.after = func() { m.Close() }
			}
			n.Store = broken
			if err := m.open(context.Background(), wrap, &Report{}); err == nil {
				t.Fatal("receipt interruption was not reported")
			}
			m.Close()
			n.Store = original
			reopened, err := Open(dir, key, n, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			calls := 0
			reopened.OnRequest = func(_ context.Context, request nostr.Event) (call.Reply, error) {
				calls++
				if call.ReadRequest(request).Body != "increment" {
					t.Error("receipt recovery changed the request")
				}
				return call.Reply{Body: "1"}, nil
			}
			// Recovery must work even if the courier is no longer available.
			for range 2 {
				if _, err := reopened.Sync(context.Background(), file.Dir{Path: t.TempDir()}); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatalf("executions after receipt recovery=%d, want one", calls)
			}
		})
	}
}

func TestReceiptJournalFailureDoesNotConsumeRequest(t *testing.T) {
	m, dir, key, n := openMail(t, nil)
	_, seal, wrap := requestWrap(t, key.Public)
	m.Close()
	if err := m.open(context.Background(), wrap, &Report{}); err == nil {
		t.Fatal("closed journal accepted a receipt")
	}
	if saved, err := n.Store.Has(seal.ID); err != nil || saved {
		t.Fatalf("receipt saved without its journal: saved=%v error=%v", saved, err)
	}
	reopened, err := Open(dir, key, n, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	calls := 0
	reopened.OnRequest = func(context.Context, nostr.Event) (call.Reply, error) {
		calls++
		return call.Reply{Body: "1"}, nil
	}
	for range 2 {
		if err := reopened.open(context.Background(), wrap, &Report{}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("executions after journal recovery=%d, want one", calls)
	}
}

func TestHistoricalReceiptIsNotExecuted(t *testing.T) {
	m, _, _, n := openMail(t, nil)
	rumor, seal, wrap := requestWrap(t, m.key.PublicKey())
	if _, err := n.Store.Save(seal); err != nil {
		t.Fatal(err)
	}
	m.OnRequest = func(context.Context, nostr.Event) (call.Reply, error) {
		t.Error("historical receipt with unknown execution was replayed")
		return call.Reply{}, nil
	}
	report := &Report{}
	if err := m.open(context.Background(), wrap, report); err != nil {
		t.Fatal(err)
	}
	if report.Answered != 1 {
		t.Fatalf("answers to historical receipt=%d, want one unknown-outcome reply", report.Answered)
	}
	var entry incoming
	if _, err := m.get(inboxBucket, rumor.ID.Hex(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.ReplySeal == "" {
		t.Fatal("historical receipt did not retain its reply")
	}
	replySeal, err := m.savedSeal(entry.ReplySeal)
	if err != nil {
		t.Fatal(err)
	}
	replyRumor, err := private.OpenOwnSeal(context.Background(), m.key, replySeal, rumor.PubKey)
	if err != nil {
		t.Fatal(err)
	}
	_, reply := call.ReadReply(replyRumor)
	if reply.Err != "outcome_unknown a previous execution did not record a result" {
		t.Fatalf("historical reply=%+v", reply)
	}
}

type refusedReceiptStore struct{ node.EventStore }

func (s refusedReceiptStore) Save(event nostr.Event) (store.Result, error) {
	if event.Kind == private.SealKind {
		return store.Result{Outcome: store.Refused, Reason: "its author deleted it"}, nil
	}
	return s.EventStore.Save(event)
}

func TestRefusedReceiptDoesNotBlockSync(t *testing.T) {
	m, _, key, n := openMail(t, nil)
	_, _, wrap := requestWrap(t, key.Public)
	original := n.Store
	n.Store = refusedReceiptStore{EventStore: original}
	report := &Report{}
	if err := m.open(context.Background(), wrap, report); err != nil {
		t.Fatal(err)
	}
	if len(report.Refused) != 1 {
		t.Fatalf("refused receipts=%v, want one", report.Refused)
	}
	n.Store = original
	calls := 0
	m.OnRequest = func(context.Context, nostr.Event) (call.Reply, error) {
		calls++
		return call.Reply{Body: "1"}, nil
	}
	if _, err := m.Sync(context.Background(), file.Dir{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("refused receipt was executed")
	}
	_, _, next := requestWrap(t, key.Public)
	if err := m.open(context.Background(), next, &Report{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("valid request after refusal: executions=%d, want one", calls)
	}
}
