package provider_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/core/provider"
)

func TestExpiredRequestDoesNotRunHandler(t *testing.T) {
	entered := make(chan struct{}, 1)
	p := startPipes(t, provider.HandlerFunc(func(context.Context, provider.Request) (string, error) { entered <- struct{}{}; return "too late", nil }))
	p.send(t, map[string]any{"op": "request", "request_id": "expired", "message": "mutate", "meta": map[string]any{}, "deadline_ms": time.Unix(1, 0).UnixMilli()})
	if reply := p.read(t); reply["op"] != "error" || reply["error"] != "provider_timeout" {
		t.Errorf("expired request = %v", reply)
	}
	select {
	case <-entered:
		t.Error("expired request reached the handler")
	default:
	}
}

type canceledCaller struct{ caller provider.Caller }

func (h *canceledCaller) SetCaller(c provider.Caller) { h.caller = c }
func (h *canceledCaller) HandleRequest(ctx context.Context, _ provider.Request) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := h.caller.Call(ctx, "x+arc://provider/", "mutate")
	if !errors.Is(err, context.Canceled) {
		return "", provider.Error("wrong_cancellation_error")
	}
	return "stopped", nil
}
func TestCanceledOutboundCallDoesNotWrite(t *testing.T) {
	p := startPipes(t, &canceledCaller{})
	p.request(t, "one", "go")
	if reply := p.read(t); reply["op"] != "reply" || reply["reply"] != "stopped" {
		t.Fatalf("a canceled call wrote to the host: %v", reply)
	}
}

func TestProviderReportsContextErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{context.Canceled, "provider_canceled"}, {context.DeadlineExceeded, "provider_timeout"}} {
		t.Run(tc.want, func(t *testing.T) {
			p := startPipes(t, provider.HandlerFunc(func(context.Context, provider.Request) (string, error) { return "", tc.err }))
			p.request(t, "one", "go")
			if reply := p.read(t); reply["error"] != tc.want {
				t.Fatalf("context failure = %v, want %s", reply, tc.want)
			}
		})
	}
}

// A pipe reader can receive a complete reply before its writer returns from
// Write. The next request may then reuse the completed request's ID.
type deliveredReply struct {
	once     sync.Once
	received chan struct{}
	release  chan struct{}
}

func (w *deliveredReply) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.received); <-w.release })
	return len(p), nil
}

func TestCompletedRequestCanReuseID(t *testing.T) {
	input, send := io.Pipe()
	output := &deliveredReply{received: make(chan struct{}), release: make(chan struct{})}
	handled := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		done <- provider.Run(context.Background(), provider.HandlerFunc(func(_ context.Context, r provider.Request) (string, error) {
			handled <- r.Message
			return r.Message, nil
		}), provider.Options{In: input, Out: output, Log: io.Discard})
	}()
	defer func() {
		close(output.release)
		send.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	if _, err := io.WriteString(send, request("same", "first", nil)); err != nil {
		t.Fatal(err)
	}
	<-output.received
	if got := <-handled; got != "first" {
		t.Fatalf("first request = %q", got)
	}
	if _, err := io.WriteString(send, request("same", "second", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-handled:
		if got != "second" {
			t.Errorf("second request = %q", got)
		}
	case <-time.After(time.Second):
		t.Error("a completed request ID remained reserved after its reply was delivered")
	}
}
