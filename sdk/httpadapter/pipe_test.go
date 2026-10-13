package httpadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/providertest"
)

var loopback = direct.Options{Loopback: true}

// openPiped opens an HTTP session on adapter with the policy of c.
func openPiped(t *testing.T, adapter *Adapter, c direct.DialConfig) (*ClientSession, error) {
	t.Helper()
	return OpenSession(t.Context(), c, "", func(_ context.Context, request string) (*session.Stream, error) {
		return providertest.Start(t, session.ServerStream, func(ctx context.Context, s *session.Stream) error {
			return adapter.HandleSession(ctx, provider.Request{From: "alice", Message: request, Meta: map[string]any{"method": "GET", "path": "/"}}, s)
		}), nil
	})
}

// TestBodyOverPipe sends the body on the pipe, and the status, the
// trailers and the count of the body in records.
func TestBodyOverPipe(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "Result")
		w.WriteHeader(203)
		_, _ = w.Write(body)
		w.Header().Set("Result", "complete")
	})
	adapter := NewWithPipes(handler, direct.AcceptConfig{Policy: direct.Auto, Options: loopback})
	piped, err := openPiped(t, adapter, direct.DialConfig{Policy: direct.DirectOnly, Options: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer piped.Close()
	if piped.Pipe == nil || piped.Path != "direct (host to host)" {
		t.Fatalf("path %q", piped.Path)
	}
	var got bytes.Buffer
	response, err := piped.Copy(&got)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 203 || response.Trailers.Get("Result") != "complete" || response.Bytes != int64(len(body)) || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("status %d, trailers %v, %d bytes", response.Status, response.Trailers, response.Bytes)
	}
}

// TestFifthPipeIsBusy holds direct.MaxPipes pipes on one adapter. The next
// offer gets busy, before a pipe opens and before the handler runs.
func TestFifthPipeIsBusy(t *testing.T) {
	release := make(chan struct{})
	calls := make(chan struct{}, 8)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	adapter := NewWithPipes(handler, direct.AcceptConfig{Policy: direct.Auto, Options: loopback})
	dial := direct.DialConfig{Policy: direct.DirectOnly, Options: loopback}
	for i := range direct.MaxPipes {
		piped, err := openPiped(t, adapter, dial)
		if err != nil {
			t.Fatalf("pipe %d: %v", i+1, err)
		}
		defer piped.Close()
		<-calls
	}
	_, err := openPiped(t, adapter, dial)
	if err == nil || !strings.Contains(err.Error(), "too many pipes") {
		t.Fatalf("pipe %d: %v, want too many pipes", direct.MaxPipes+1, err)
	}
	if len(calls) != 0 {
		t.Fatal("the handler ran for the refused offer")
	}
	close(release)
}

// TestNoPipeAndAuto falls back to session frames when the provider opens no
// pipe, and tells the user. Another policy stops.
func TestNoPipeAndAuto(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "frames") })
	adapter := NewWithPipes(handler, direct.AcceptConfig{Policy: direct.Off})
	var notes bytes.Buffer
	piped, err := openPiped(t, adapter, direct.DialConfig{Policy: direct.Auto, Options: loopback, Notes: &notes})
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := piped.Copy(&got); err != nil || got.String() != "frames" || piped.Path != direct.SessionPath {
		t.Fatalf("%q on %q: %v", got.String(), piped.Path, err)
	}
	if !strings.Contains(notes.String(), "the provider has no pipe; using session frames over the relay") {
		t.Fatalf("notes: %q", notes.String())
	}
	if _, err := openPiped(t, adapter, direct.DialConfig{Policy: direct.DirectOnly, Options: loopback}); err == nil || !strings.Contains(err.Error(), "--path session") {
		t.Fatalf("policy direct with a provider that has no pipe: %v", err)
	}
}

// TestOfferOverLimit falls back to session frames in auto when the host of
// the provider refuses the request with the offer as request_too_large. A
// session request holds at most 16 KiB, and the offer adds 1 to 3 KiB.
func TestOfferOverLimit(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "frames") })
	adapter := NewWithPipes(handler, direct.AcceptConfig{Policy: direct.Auto, Options: loopback})
	var notes bytes.Buffer
	piped, err := OpenSession(t.Context(), direct.DialConfig{Policy: direct.Auto, Options: loopback, Notes: &notes}, "", func(_ context.Context, request string) (*session.Stream, error) {
		return providertest.Start(t, session.ServerStream, func(ctx context.Context, s *session.Stream) error {
			if strings.Contains(request, `"direct"`) {
				return provider.ErrRequestTooLarge
			}
			return adapter.HandleSession(ctx, provider.Request{From: "alice", Message: request, Meta: map[string]any{"method": "GET", "path": "/"}}, s)
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := piped.Copy(&got); err != nil || got.String() != "frames" || piped.Path != direct.SessionPath {
		t.Fatalf("%q on %q: %v", got.String(), piped.Path, err)
	}
	if !strings.Contains(notes.String(), "over the limit of the provider; using session frames over the relay") {
		t.Fatalf("notes: %q", notes.String())
	}
}

// TestRefusalBeforeOpenIsConfirmed falls back to session frames in auto when
// an older provider refuses the offer as invalid_request before the
// transport confirms the open. The opener then gives the refusal as its
// error. The policy direct tells the user to use session frames.
func TestRefusalBeforeOpenIsConfirmed(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "frames") })
	adapter := NewWithPipes(handler, direct.AcceptConfig{Policy: direct.Auto, Options: loopback})
	open := func(_ context.Context, request string) (*session.Stream, error) {
		if strings.Contains(request, `"direct"`) {
			// The error of core/call when the answer ends the open.
			return nil, fmt.Errorf("session open failed; outcome may be unknown: %w", session.RemoteError("invalid_request"))
		}
		return providertest.Start(t, session.ServerStream, func(ctx context.Context, s *session.Stream) error {
			return adapter.HandleSession(ctx, provider.Request{From: "alice", Message: request, Meta: map[string]any{"method": "GET", "path": "/"}}, s)
		}), nil
	}
	var notes bytes.Buffer
	piped, err := OpenSession(t.Context(), direct.DialConfig{Policy: direct.Auto, Options: loopback, Notes: &notes}, "", open)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := piped.Copy(&got); err != nil || got.String() != "frames" || piped.Path != direct.SessionPath {
		t.Fatalf("%q on %q: %v", got.String(), piped.Path, err)
	}
	if !strings.Contains(notes.String(), "the provider has no pipe; using session frames over the relay") {
		t.Fatalf("notes: %q", notes.String())
	}
	if _, err := OpenSession(t.Context(), direct.DialConfig{Policy: direct.DirectOnly, Options: loopback}, "", open); err == nil || !strings.Contains(err.Error(), "--path session") {
		t.Fatalf("policy direct with a provider that has no pipe: %v", err)
	}
}

// openUpload opens a duplex stream_body session on adapter with the policy
// of c.
func openUpload(t *testing.T, adapter *Adapter, c direct.DialConfig) (*ClientSession, error) {
	t.Helper()
	return OpenSession(t.Context(), c, `{"stream_body":true}`, func(_ context.Context, request string) (*session.Stream, error) {
		return providertest.Start(t, session.Duplex, func(ctx context.Context, s *session.Stream) error {
			return adapter.HandleSession(ctx, provider.Request{From: "alice", Message: request, Meta: map[string]any{"method": "POST", "path": "/"}}, s)
		}), nil
	})
}

// sumHandler answers with the number of request body bytes and their
// SHA-256.
var sumHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	sum := sha256.New()
	n, err := io.Copy(sum, r.Body)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	_, _ = fmt.Fprintf(w, "%d %x", n, sum.Sum(nil))
})

// TestUploadOverPipe sends a request body on the pipe. The handler gets
// each byte, and the "end" record counts them.
func TestUploadOverPipe(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<19)
	adapter := NewWithPipes(sumHandler, direct.AcceptConfig{Policy: direct.Auto, Options: loopback})
	piped, err := openUpload(t, adapter, direct.DialConfig{Policy: direct.DirectOnly, Options: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer piped.Close()
	if piped.Pipe == nil {
		t.Fatalf("path %q", piped.Path)
	}
	piped.Body = bytes.NewReader(body)
	var got bytes.Buffer
	response, err := piped.Copy(&got)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d %x", len(body), sha256.Sum256(body))
	if got.String() != want || response.RequestBytes != int64(len(body)) || response.RequestSent != int64(len(body)) {
		t.Fatalf("handler %q, want %q; request %d of %d bytes", got.String(), want, response.RequestBytes, response.RequestSent)
	}
}

// TestUploadAnsweredEarly has a handler that answers 413 before it reads
// the body. The provider closes the pipe after the "end" record. That is
// the answer of the handler, not a broken pipe.
func TestUploadAnsweredEarly(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})
	adapter := NewWithPipes(handler, direct.AcceptConfig{Policy: direct.Auto, Options: loopback})
	piped, err := openUpload(t, adapter, direct.DialConfig{Policy: direct.DirectOnly, Options: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer piped.Close()
	piped.Body = io.LimitReader(zeros{}, 64<<20)
	response, err := piped.Copy(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 413 || response.RequestBytes >= 64<<20 || response.RequestBytes > response.RequestSent {
		t.Fatalf("status %d, request %d of %d bytes", response.Status, response.RequestBytes, response.RequestSent)
	}
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) { clear(b); return len(b), nil }

// TestFifthUploadPipeIsBusy holds direct.MaxPipes uploads on one adapter.
// The next upload offer gets busy before the handler runs.
func TestFifthUploadPipeIsBusy(t *testing.T) {
	calls := make(chan struct{}, 8)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		_, _ = io.Copy(io.Discard, r.Body)
	})
	adapter := NewWithPipes(handler, direct.AcceptConfig{Policy: direct.Auto, Options: loopback})
	dial := direct.DialConfig{Policy: direct.DirectOnly, Options: loopback}
	for i := range direct.MaxPipes {
		piped, err := openUpload(t, adapter, dial)
		if err != nil {
			t.Fatalf("pipe %d: %v", i+1, err)
		}
		defer piped.Close()
		// The body never ends, so the pipe stays open.
		reader, writer := io.Pipe()
		defer writer.Close()
		piped.Body = reader
		go func() { _, _ = piped.Copy(io.Discard) }()
		<-calls
	}
	_, err := openUpload(t, adapter, dial)
	if err == nil || !strings.Contains(err.Error(), "too many pipes") {
		t.Fatalf("pipe %d: %v, want too many pipes", direct.MaxPipes+1, err)
	}
	if len(calls) != 0 {
		t.Fatal("the handler ran for the refused offer")
	}
}
