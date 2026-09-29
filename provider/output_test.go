package provider_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gezibash/arc/provider"
)

type observedOutput struct {
	io.WriteCloser
	writes chan struct{}
}

func (o *observedOutput) Write(body []byte) (int, error) {
	o.writes <- struct{}{}
	return o.WriteCloser.Write(body)
}

type outputHarness struct {
	input  *io.PipeWriter
	reader *bufio.Reader
	writes <-chan struct{}
	done   <-chan error
}

func runWithUnreadOutput(t *testing.T, ctx context.Context, handler provider.Handler, opts provider.Options) *outputHarness {
	t.Helper()
	input, send := io.Pipe()
	read, output := io.Pipe()
	observed := &observedOutput{WriteCloser: output, writes: make(chan struct{}, 8)}
	done := make(chan error, 1)
	finished := make(chan struct{})
	opts.In, opts.Out, opts.Log = input, observed, io.Discard
	go func() {
		done <- provider.Run(ctx, handler, opts)
		output.Close()
		close(finished)
	}()
	t.Cleanup(func() {
		send.Close()
		read.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("runtime did not stop after closing test pipes")
		}
	})
	return &outputHarness{input: send, reader: bufio.NewReader(read), writes: observed.writes, done: done}
}
func waitForWrite(t *testing.T, p *outputHarness) {
	t.Helper()
	select {
	case <-p.writes:
	case <-time.After(time.Second):
		t.Fatal("runtime did not attempt output")
	}
}
func awaitOutputResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("output backpressure prevented cancellation")
		return nil
	}
}

func TestBlockedOutputStopsOnCancellation(t *testing.T) {
	for _, end := range []string{"context", "EOF"} {
		t.Run(end, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := runWithUnreadOutput(t, ctx, echo{}, provider.Options{})
			if _, err := io.WriteString(p.input, request("reply", "hello", nil)); err != nil {
				t.Fatal(err)
			}
			waitForWrite(t, p)
			want := context.Canceled
			if end == "context" {
				cancel()
			} else {
				p.input.Close()
				want = context.DeadlineExceeded
			}
			if err := awaitOutputResult(t, p.done); !errors.Is(err, want) {
				t.Fatalf("Run=%v, want %v", err, want)
			}
		})
	}
}

type capturedCaller struct{ ready chan provider.Caller }

func (h *capturedCaller) SetCaller(c provider.Caller) { h.ready <- c }
func (*capturedCaller) HandleRequest(context.Context, provider.Request) (string, error) {
	return "", nil
}

func TestOutboundOutputCancellation(t *testing.T) {
	for _, stage := range []string{"call", "cancel_notice", "waiting_writer"} {
		t.Run(stage, func(t *testing.T) {
			runCtx, stop := context.WithCancel(context.Background())
			defer stop()
			h := &capturedCaller{ready: make(chan provider.Caller, 1)}
			p := runWithUnreadOutput(t, runCtx, h, provider.Options{})
			caller := <-h.ready
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := caller.Call(ctx, "echo+arc://provider/", "first"); done <- err }()
			waitForWrite(t, p)
			if stage == "waiting_writer" {
				queued, release := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer release()
				queuedDone := make(chan error, 1)
				go func() { _, err := caller.Call(queued, "echo+arc://provider/", "must not send"); queuedDone <- err }()
				if err := awaitOutputResult(t, queuedDone); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("queued call=%v", err)
				}
				line, err := p.reader.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				var sent map[string]any
				if err := json.Unmarshal(line, &sent); err != nil {
					t.Fatal(err)
				}
				result, _ := json.Marshal(map[string]any{"op": "result", "call_id": sent["call_id"], "reply": "ok"})
				if _, err := p.input.Write(append(result, '\n')); err != nil {
					t.Fatal(err)
				}
				if err := awaitOutputResult(t, done); err != nil {
					t.Fatalf("canceled waiter damaged active write: %v", err)
				}
				select {
				case <-p.writes:
					t.Fatal("canceled waiter wrote a call")
				default:
				}
				return
			}
			if stage == "cancel_notice" {
				if _, err := p.reader.ReadBytes('\n'); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			if stage == "cancel_notice" {
				waitForWrite(t, p)
			}
			if err := awaitOutputResult(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("Call=%v, want cancellation", err)
			}
			// An interrupted active write makes the protocol stream unusable.
			if err := awaitOutputResult(t, p.done); err == nil {
				t.Fatal("broken output stream reported success")
			}
		})
	}
}

type partialOutput struct {
	writes atomic.Int32
	err    error
}

func (w *partialOutput) Write([]byte) (int, error) { w.writes.Add(1); return 1, w.err }

func TestPartialOutputFailureStopsTheRuntime(t *testing.T) {
	input, send := io.Pipe()
	t.Cleanup(func() { input.Close(); send.Close() })
	failure := errors.New("output disconnected after one byte")
	output := &partialOutput{err: failure}
	done := make(chan error, 1)
	go func() {
		done <- provider.Run(context.Background(), echo{}, provider.Options{In: input, Out: output, Log: io.Discard})
	}()
	if _, err := io.WriteString(send, request("one", "hello", nil)); err != nil {
		t.Fatal(err)
	}
	if err := awaitOutputResult(t, done); !errors.Is(err, failure) {
		t.Fatalf("Run=%v, want output failure", err)
	}
	if got := output.writes.Load(); got != 1 {
		t.Fatalf("writes after partial failure=%d, want one", got)
	}
}

func TestOSPipeOutputStopsOnCancellation(t *testing.T) {
	input, send := io.Pipe()
	read, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer output.Close()
	defer send.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &observedOutput{WriteCloser: output, writes: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { done <- provider.Run(ctx, echo{}, provider.Options{In: input, Out: observed, Log: io.Discard}) }()
	// The reply exceeds the OS pipe buffer. No reader drains it.
	if _, err := io.WriteString(send, request("large", strings.Repeat("x", 2*1024*1024), nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed.writes:
	case <-time.After(time.Second):
		t.Fatal("no OS pipe write")
	}
	cancel()
	if err := awaitOutputResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v, want cancellation", err)
	}
}
