package call_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/provider/host"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/internal/testrelay"
)

func liveSessionProvider(t *testing.T, ctx context.Context, k keys.Key, r relay.Relay, modes ...session.Mode) *call.Server {
	t.Helper()
	process, err := host.Start(echoBinary, nil, "", nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.Stop() })
	server := call.NewServer(k, "primary", process, 64*1024, nil, quiet, modes...)
	ready := make(chan struct{})
	go server.ServeLive(ctx, r, func() { close(ready) })
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return server
}
func openSession(t *testing.T, ctx context.Context, client, provider keys.Key, r relay.Relay, mode session.Mode, body string) *session.Stream {
	t.Helper()
	s, err := call.OpenSession(ctx, client, provider.Public, call.Request{Capability: "primary", Method: "ECHO", Path: "/", Body: body}, mode, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func line(t *testing.T, r *bufio.Reader, want string) {
	t.Helper()
	got, err := r.ReadString('\n')
	if err != nil || got != want {
		t.Fatalf("line = %q, %v; want %q", got, err, want)
	}
}
func TestSessionIsInteractiveIsolatedAndIdentityBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := relay.Relay{URL: testrelay.Start(t)}
	owner := keys.Generate()
	client := keys.Generate()
	liveSessionProvider(t, ctx, owner, r, session.RequestReply, session.Duplex)
	s := openSession(t, ctx, client, owner, r, session.Duplex, "")
	reader := bufio.NewReader(s)
	line(t, reader, "ready\n")
	// A different authenticated participant cannot write to this session by ID.
	attacker := keys.Generate()
	f := session.Frame{Version: 1, ID: s.ID(), Op: "data", Seq: 1, Data: []byte("SET hijacked\n")}
	body, _ := json.Marshal(map[string]any{"frame": f})
	rumor := private.Rumor(attacker, call.SessionKind, string(body), nostr.Tags{{"p", owner.Public.Hex()}, {"session", s.ID()}}, time.Now())
	wrap, err := private.Wrap(ctx, attacker, owner.Public, rumor, private.RelayForm, private.LiveWrapKind, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Send(ctx, wrap); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(s, "GET\n"); err != nil {
		t.Fatal(err)
	}
	line(t, reader, "empty\n")
	if _, err = io.WriteString(s, "SET remembered\n"); err != nil {
		t.Fatal(err)
	}
	line(t, reader, "remembered\n")
	if _, err = io.WriteString(s, "GET\n"); err != nil {
		t.Fatal(err)
	}
	line(t, reader, "remembered\n")
	other := openSession(t, ctx, client, owner, r, session.Duplex, "")
	otherReader := bufio.NewReader(other)
	line(t, otherReader, "ready\n")
	if _, err = io.WriteString(other, "GET\n"); err != nil {
		t.Fatal(err)
	}
	line(t, otherReader, "empty\n")
	if err = s.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("final close = %v", err)
	}
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
	// Canceling a session leaves ordinary calls on the shared provider intact.
	reply, _, err := call.Live(ctx, client, owner.Public, call.Request{Capability: "primary", Method: "ECHO", Path: "/", Body: "still working"}, r)
	if err != nil || reply.Body != "ECHO / still working" {
		t.Fatalf("ordinary call = %+v, %v", reply, err)
	}
}

func TestSessionModesAndProviderRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := relay.Relay{URL: testrelay.Start(t)}
	owner := keys.Generate()
	client := keys.Generate()
	liveSessionProvider(t, ctx, owner, r, session.RequestReply, session.ServerStream)
	s := openSession(t, ctx, client, owner, r, session.ServerStream, "")
	body, err := io.ReadAll(s)
	if err != nil || string(body) != "first\nsecond\n" {
		t.Fatalf("stream = %q, %v", body, err)
	}
	_, err = call.OpenSession(ctx, client, owner.Public, call.Request{Capability: "primary"}, session.Duplex, r)
	if err == nil || !strings.Contains(err.Error(), "interaction_not_supported") {
		t.Fatalf("undeclared duplex = %v", err)
	}
	denied := openSession(t, ctx, client, owner, r, session.ServerStream, "deny")
	if _, err = io.ReadAll(denied); err == nil || err.Error() != "unauthorized" {
		t.Fatalf("provider refusal = %v", err)
	}
	// A complete request/reply result may be split into bounded transport chunks.
	text := strings.Repeat("x", session.MaxChunk)
	unary := openSession(t, ctx, client, owner, r, session.RequestReply, text)
	body, err = io.ReadAll(unary)
	if err != nil || string(body) != "ECHO / "+text {
		t.Fatalf("fragmented reply length = %d, %v", len(body), err)
	}
}

func TestProviderConsumesAnotherStreamingProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := relay.Relay{URL: testrelay.Start(t)}
	front := keys.Generate()
	back := keys.Generate()
	client := keys.Generate()
	liveSessionProvider(t, ctx, back, r, session.ServerStream, session.Duplex)
	server := liveSessionProvider(t, ctx, front, r, session.ServerStream, session.Duplex)
	called := make(chan call.Outbound, 2)
	server.SetSessionCaller(func(ctx context.Context, out call.Outbound, mode session.Mode) (*session.Stream, error) {
		called <- out
		body := ""
		if strings.HasSuffix(out.Address, "/deny") {
			body = "deny"
		}
		return call.OpenSession(ctx, front, back.Public, call.Request{Capability: "primary", Body: body}, mode, r)
	})
	denied := openSession(t, ctx, client, front, r, session.ServerStream, "session echo+arc://backend/deny")
	if _, err := io.ReadAll(denied); err == nil || err.Error() != "unauthorized" {
		t.Fatalf("nested refusal = %v", err)
	}
	<-called
	for _, mode := range []session.Mode{session.ServerStream, session.Duplex} {
		s := openSession(t, ctx, client, front, r, mode, "session echo+arc://backend/")
		if mode == session.Duplex {
			reader := bufio.NewReader(s)
			line(t, reader, "ready\n")
			if _, err := io.WriteString(s, "SET through both providers\n"); err != nil {
				t.Fatal(err)
			}
			line(t, reader, "through both providers\n")
			if err := s.CloseWrite(); err != nil {
				t.Fatal(err)
			}
		}
		result, err := io.ReadAll(s)
		if err != nil {
			t.Fatal(err)
		}
		if mode == session.ServerStream && string(result) != "first\nsecond\n" {
			t.Fatalf("nested output = %q", result)
		}
		if out := <-called; out.Address != "echo+arc://backend/" {
			t.Fatalf("nested target = %+v", out)
		}
	}
}

type watchLoss struct {
	relay.Relay
	lost chan struct{}
}

func (w watchLoss) Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error) {
	events, err := w.Relay.Watch(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make(chan nostr.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.lost:
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				case <-w.lost:
					return
				}
			}
		}
	}()
	return out, nil
}
func TestSessionWatchLossIsExplicitAndDoesNotResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := relay.Relay{URL: testrelay.Start(t)}
	owner := keys.Generate()
	client := keys.Generate()
	liveSessionProvider(t, ctx, owner, r, session.Duplex)
	link := watchLoss{r, make(chan struct{})}
	s, err := call.OpenSession(ctx, client, owner.Public, call.Request{Capability: "primary"}, session.Duplex, link)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	line(t, bufio.NewReader(s), "ready\n")
	close(link.lost)
	if _, err = io.ReadAll(s); !errors.Is(err, session.ErrDisconnected) {
		t.Fatalf("watch loss = %v", err)
	}
	if _, err = s.Write([]byte("must not resume\n")); !errors.Is(err, session.ErrDisconnected) {
		t.Fatalf("write after disconnect = %v", err)
	}
}
