package provider_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/provider"
)

func TestRejectedOutputDoesNotBlockControl(t *testing.T) {
	for _, rejection := range []string{"busy", "invalid", "oversized", "expired", "duplicate"} {
		for _, end := range []string{"cancel", "EOF"} {
			t.Run(rejection+"/"+end, func(t *testing.T) {
				ctx := t.Context()
				entered, canceled := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				handler := provider.HandlerFunc(func(ctx context.Context, _ provider.Request) (string, error) {
					if calls.Add(1) != 1 {
						t.Error("rejected request reached the handler")
						return "unexpected", nil
					}
					close(entered)
					<-ctx.Done()
					close(canceled)
					return "stopped", nil
				})
				opts := provider.Options{MaxConcurrent: 2, MaxLineBytes: 1024}
				if rejection == "busy" {
					opts.MaxConcurrent = 1
				}
				p := runWithUnreadOutput(t, ctx, handler, opts)
				if _, err := io.WriteString(p.input, request("first", "wait", nil)); err != nil {
					t.Fatal(err)
				}
				<-entered
				line := request("excess", "must not execute", nil)
				switch rejection {
				case "invalid":
					line = "not json\n"
				case "oversized":
					line = strings.Repeat("x", 2048) + "\n"
				case "expired":
					line = fmt.Sprintf("{\"op\":\"request\",\"request_id\":\"expired\",\"message\":\"wait\",\"meta\":{},\"deadline_ms\":%d}\n", time.Now().Add(-time.Hour).UnixMilli())
				case "duplicate":
					line = request("first", "duplicate", nil)
				}
				if _, err := io.WriteString(p.input, line); err != nil {
					t.Fatal(err)
				}
				waitForWrite(t, p)
				if end == "EOF" {
					p.input.Close()
				} else if _, err := io.WriteString(p.input, "{\"op\":\"cancel\",\"request_id\":\"first\"}\n"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-canceled:
				case <-time.After(500 * time.Millisecond):
					t.Fatal("blocked rejection prevented handler cancellation")
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("handler executions=%d, want one", got)
				}
				if end == "EOF" {
					if err := awaitOutputResult(t, p.done); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("EOF with unread output=%v", err)
					}
				}
			})
		}
	}
}

func TestRejectionBacklogIsBounded(t *testing.T) {
	ctx := t.Context()
	entered := make(chan struct{})
	p := runWithUnreadOutput(t, ctx, provider.HandlerFunc(func(ctx context.Context, _ provider.Request) (string, error) {
		close(entered)
		<-ctx.Done()
		return "stopped", nil
	}), provider.Options{MaxConcurrent: 1})
	if _, err := io.WriteString(p.input, request("first", "wait", nil)); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := io.WriteString(p.input, request("second", "reject", nil)); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, p)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_, _ = io.WriteString(p.input, strings.Repeat(request("excess", "reject", nil), 1000))
	}()
	if err := awaitOutputResult(t, p.done); !errors.Is(err, provider.ErrBusy) {
		t.Fatalf("unread rejection overflow=%v, want provider_busy", err)
	}
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("overflow did not close input")
	}
}
