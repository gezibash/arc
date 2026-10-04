package mail

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/file"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
)

func receivedRequest(t *testing.T, m *Mail) nostr.Event {
	t.Helper()
	sender := keys.Generate()
	request := call.RequestRumor(sender, m.key.PublicKey(), call.Request{Body: "increment"}, time.Now())
	seal, err := private.Seal(context.Background(), sender, m.key.PublicKey(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.node.Store.Save(seal); err != nil {
		t.Fatal(err)
	}
	if err := m.receiveRequest(context.Background(), private.Opened{Seal: seal, Rumor: request}, false, &Report{}); err != nil {
		t.Fatal(err)
	}
	return request
}

type failedRead struct {
	node.EventStore
	err error
}

func (s failedRead) Query(nostr.Filter) ([]nostr.Event, error) { return nil, s.err }

func TestPendingRequestRecoversFromPreparationFailure(t *testing.T) {
	for _, failure := range []string{"signer", "store", "cancellation", "cancellation_during_open"} {
		t.Run(failure, func(t *testing.T) {
			m, dir, key, n := openMail(t, nil)
			receivedRequest(t, m)
			called := 0
			handler := func(_ context.Context, request nostr.Event) (call.Reply, error) {
				called++
				if call.ReadRequest(request).Body != "increment" {
					t.Error("request changed during recovery")
				}
				return call.Reply{Body: "1"}, nil
			}
			m.OnRequest = handler
			ctx := context.Background()
			want := errors.New("temporary preparation failure")
			original := n.Store
			switch failure {
			case "signer":
				m.key = deniedDecrypt{Key: key, err: want}
			case "store":
				n.Store = failedRead{EventStore: original, err: want}
			case "cancellation":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "cancellation_during_open":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				m.key = cancelAfterDecrypt{Key: key, cancel: cancel}
				want = context.Canceled
			}
			if _, err := m.Sync(ctx, file.Dir{Path: t.TempDir()}); !errors.Is(err, want) {
				t.Fatalf("preparation error=%v, want %v", err, want)
			}
			if called != 0 {
				t.Fatal("failed preparation invoked the handler")
			}
			n.Store = original
			m.Close()
			reopened, err := openDiskMail(dir, key, n, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			reopened.OnRequest = handler
			for range 2 {
				if _, err := reopened.Sync(context.Background(), file.Dir{Path: t.TempDir()}); err != nil {
					t.Fatal(err)
				}
			}
			if called != 1 {
				t.Fatalf("executions after recovery=%d, want exactly one", called)
			}
		})
	}
}

type preparingSigner struct {
	keys.Key
	entered chan struct{}
	release chan struct{}
}

func (s preparingSigner) Decrypt(ctx context.Context, body string, from nostr.PubKey) (string, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.Key.Decrypt(ctx, body, from)
}

func TestConcurrentPreparationExecutesOnce(t *testing.T) {
	m, dir, key, n := openMail(t, nil)
	request := receivedRequest(t, m)
	signer := preparingSigner{Key: key, entered: make(chan struct{}, 2), release: make(chan struct{})}
	defer close(signer.release)
	m.key = signer
	other, err := openDiskMail(dir, signer, n, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var called atomic.Int32
	handler := func(context.Context, nostr.Event) (call.Reply, error) {
		called.Add(1)
		return call.Reply{Body: "1"}, nil
	}
	m.OnRequest, other.OnRequest = handler, handler
	done := make(chan error, 2)
	for _, receiver := range []*Mail{m, other} {
		go func() { done <- receiver.answer(context.Background(), request.ID.Hex(), &Report{}) }()
	}
	for range 2 {
		select {
		case <-signer.entered:
		case <-time.After(time.Second):
			t.Fatal("both readers did not prepare the pending request")
		}
	}
	// Release both preparations together. Only one may claim execution.
	signer.release <- struct{}{}
	signer.release <- struct{}{}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("preparation did not finish")
		}
	}
	if got := called.Load(); got != 1 {
		t.Fatalf("concurrent executions=%d, want one", got)
	}
}

func TestIncomingMailReadErrorsAreReturned(t *testing.T) {
	m, _, key, n := openMail(t, nil)
	sender := keys.Generate()
	rumor := private.Rumor(sender, MessageKind, "received text", nostr.Tags{}, time.Now())
	seal, err := private.Seal(context.Background(), sender, key.Public, rumor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Store.Save(seal); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Inbox(context.Background()); err != nil || len(got) != 1 || got[0].Text != "received text" {
		t.Fatalf("baseline Inbox=%v, %v", got, err)
	}
	if got, err := m.Rumors(context.Background(), []nostr.Kind{MessageKind}); err != nil || len(got) != 1 {
		t.Fatalf("baseline Rumors=%v, %v", got, err)
	}
	for _, failure := range []string{"signer", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			m.key = key
			ctx := context.Background()
			want := errors.New("signer refused decryption")
			if failure == "signer" {
				m.key = deniedDecrypt{Key: key, err: want}
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			if _, err := m.Inbox(ctx); !errors.Is(err, want) {
				t.Errorf("Inbox error=%v, want %v", err, want)
			}
			if _, err := m.Rumors(ctx, []nostr.Kind{MessageKind}); !errors.Is(err, want) {
				t.Errorf("Rumors error=%v, want %v", err, want)
			}
		})
	}
}

type cancelAfterDecrypt struct {
	keys.Key
	cancel context.CancelFunc
}

func (s cancelAfterDecrypt) Decrypt(ctx context.Context, body string, from nostr.PubKey) (string, error) {
	text, err := s.Key.Decrypt(ctx, body, from)
	s.cancel()
	return text, err
}
