package provider_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gezibash/arc/provider"
)

func TestEOFStopsActiveWorkBeforeJoining(t *testing.T) {
	entered := make(chan struct{})
	p := startPipes(t, provider.HandlerFunc(func(ctx context.Context, _ provider.Request) (string, error) {
		close(entered)
		<-ctx.Done()
		return "stopped", nil
	}))
	p.request(t, "one", "wait")
	<-entered
	p.in.Close()
	if reply := p.read(t); reply["reply"] != "stopped" {
		t.Fatalf("reply=%v", reply)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("EOF did not finish a context-aware handler")
	}
}

func TestCancellationInterruptsIdleInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPipesWithOptions(t, ctx, provider.HandlerFunc(func(context.Context, provider.Request) (string, error) {
		t.Error("no request was sent")
		return "", nil
	}), provider.Options{})
	cancel()
	select {
	case err := <-p.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run remained blocked reading idle input")
	}
}

func TestFullAdmissionStillAcceptsCancellation(t *testing.T) {
	entered := make(chan struct{}, 2)
	p := startPipesWithOptions(t, context.Background(), provider.HandlerFunc(func(ctx context.Context, _ provider.Request) (string, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return "canceled", nil
	}), provider.Options{MaxConcurrent: 1})
	p.request(t, "first", "wait")
	<-entered
	p.request(t, "second", "must not execute")
	if reply := p.read(t); reply["request_id"] != "second" || reply["error"] != "provider_busy" {
		t.Fatalf("excess request=%v", reply)
	}
	p.send(t, map[string]any{"op": "cancel", "request_id": "first"})
	if reply := p.read(t); reply["request_id"] != "first" || reply["reply"] != "canceled" {
		t.Fatalf("cancel result=%v", reply)
	}
	select {
	case <-entered:
		t.Fatal("excess work reached the handler")
	default:
	}
}

func TestRequestCarriesTheCallersDeadline(t *testing.T) {
	deadline := time.Now().Add(100 * time.Millisecond).UnixMilli()
	observed := make(chan time.Time, 1)
	p := startPipes(t, provider.HandlerFunc(func(ctx context.Context, _ provider.Request) (string, error) {
		d, _ := ctx.Deadline()
		observed <- d
		<-ctx.Done()
		return "expired", nil
	}))
	p.send(t, map[string]any{"op": "request", "request_id": "one", "message": "wait", "meta": map[string]any{}, "deadline_ms": deadline})
	if got := <-observed; got.UnixMilli() != deadline {
		t.Fatalf("deadline=%v, want %v", got, time.UnixMilli(deadline))
	}
	if reply := p.read(t); reply["reply"] != "expired" {
		t.Fatalf("reply=%v", reply)
	}
}

func TestCancelingARequestCancelsItsOutboundCall(t *testing.T) {
	p := startPipes(t, &calling{})
	p.request(t, "parent", "x+arc://provider/")
	call := p.read(t)
	if call["op"] != "call" {
		t.Fatalf("outbound=%v", call)
	}
	p.send(t, map[string]any{"op": "cancel", "request_id": "parent"})
	stop := p.read(t)
	if stop["op"] != "cancel" || stop["call_id"] != call["call_id"] {
		t.Fatalf("outbound cancellation=%v", stop)
	}
	if reply := p.read(t); reply["request_id"] != "parent" {
		t.Fatalf("reply=%v", reply)
	}
	p.request(t, "next", "echo")
	if reply := p.read(t); reply["request_id"] != "next" || reply["reply"] != "echo" {
		t.Fatalf("request cancellation damaged the shared stream: %v", reply)
	}
}
