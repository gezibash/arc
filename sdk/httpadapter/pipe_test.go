package httpadapter

import (
	"bytes"
	"context"
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
