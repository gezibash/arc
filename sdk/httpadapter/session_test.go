package httpadapter

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/providertest"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func httpStream(t *testing.T, handler http.Handler, mode session.Mode, method, body string) *session.Stream {
	return providertest.Start(t, mode, func(ctx context.Context, s *session.Stream) error {
		return New(handler).HandleSession(ctx, provider.Request{From: "alice", Message: body, Meta: map[string]any{"method": method, "path": "/test"}}, s)
	})
}
func httpRecord(t *testing.T, r *SessionReader) SessionRecord {
	t.Helper()
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestHTTPStreamFlushHeadersTrailersAndLargeBody(t *testing.T) {
	next := make(chan struct{})
	ctx := t.Context()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(CallerHeader) != "alice" {
			http.Error(w, "forged", 403)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "Result")
		w.WriteHeader(201)
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-next:
		case <-ctx.Done():
			return
		}
		io.WriteString(w, strings.Repeat("x", MaxHTTPBody+1))
		w.Header().Set("Result", "complete")
	})
	stream := httpStream(t, handler, session.ServerStream, "GET", `{"headers":{"Arc-Caller":["forged"]}}`)
	rd := NewSessionReader(stream)
	head := httpRecord(t, rd)
	if head.Type != "response" || head.Status != 201 {
		t.Fatal(head)
	}
	first := httpRecord(t, rd)
	if string(first.Data) != "data: first\n\n" {
		t.Fatalf("flush: %+v", first)
	}
	close(next)
	total := 0
	trailer := ""
	ended := false
	for {
		v, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		total += len(v.Data)
		if v.Type == "trailers" {
			trailer = v.Headers.Get("Result")
		}
		if v.Type == "end" {
			ended = true
		}
	}
	if total != MaxHTTPBody+1 || trailer != "complete" || !ended || stream.Wait() != nil {
		t.Fatalf("body=%d trailer=%s ended=%v", total, trailer, ended)
	}
}
func TestHTTPDuplexRequestBodyAndCancellation(t *testing.T) {
	stopped := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		buf := make([]byte, 32)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				w.(http.Flusher).Flush()
			}
			if err != nil {
				return
			}
		}
	})
	stream := httpStream(t, handler, session.Duplex, "POST", `{"stream_body":true}`)
	rd := NewSessionReader(stream)
	if v := httpRecord(t, rd); v.Status != 200 {
		t.Fatal(v)
	}
	if _, err := io.WriteString(stream, "hello"); err != nil {
		t.Fatal(err)
	}
	if v := httpRecord(t, rd); string(v.Data) != "hello" {
		t.Fatal(v)
	}
	_ = stream.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler retained canceled body")
	}
}
func TestHTTPWebSocketMessagesPingAndClose(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(CallerHeader) != "alice" {
			http.Error(w, "forged", 403)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"echo"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err = conn.Write(r.Context(), kind, data); err != nil {
				return
			}
		}
	})
	stream := httpStream(t, handler, session.Duplex, "GET", `{"websocket":true,"subprotocols":["echo"],"headers":{"Arc-Caller":["forged"]}}`)
	rd := NewSessionReader(stream)
	head := httpRecord(t, rd)
	if head.Status != 101 || head.Protocol != "echo" {
		t.Fatal(head)
	}
	encoder := json.NewEncoder(stream)
	for _, record := range []SessionRecord{{Type: "text", Text: "hello"}, {Type: "binary", Data: []byte{0, 255, 1}}, {Type: "ping"}} {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
		got := httpRecord(t, rd)
		if record.Type == "ping" {
			if got.Type != "pong" {
				t.Fatal(got)
			}
		} else if got.Type != record.Type || got.Text != record.Text || string(got.Data) != string(record.Data) {
			t.Fatalf("echo %+v -> %+v", record, got)
		}
	}
	if err := encoder.Encode(SessionRecord{Type: "close", Code: 1000, Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	closed := httpRecord(t, rd)
	if closed.Type != "ws_close" || closed.Code != 1000 {
		t.Fatal(closed)
	}
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatal(err)
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
}
func TestHTTPWebSocketHandshakeRefusal(t *testing.T) {
	stream := httpStream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) }), session.Duplex, "GET", `{"websocket":true}`)
	head := httpRecord(t, NewSessionReader(stream))
	if head.Status != 403 {
		t.Fatal(head)
	}
	if _, err := io.ReadAll(stream); err == nil || err.Error() != "websocket_handshake_failed" {
		t.Fatalf("handshake failure: %v", err)
	}
}
