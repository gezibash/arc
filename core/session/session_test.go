package session_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gezibash/arc/core/session"
)

func pair(t *testing.T, mode session.Mode) (*session.Stream, *session.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	var client, server *session.Stream
	var err error
	id := session.ID()
	client, err = session.New(ctx, id, mode, true, func(_ context.Context, f session.Frame) error { return server.Receive(f) })
	if err != nil {
		t.Fatal(err)
	}
	server, err = session.New(ctx, id, mode, false, func(_ context.Context, f session.Frame) error { return client.Receive(f) })
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Accept(); err != nil {
		t.Fatal(err)
	}
	return client, server
}

func TestCreditHalfCloseAndIndependentOutput(t *testing.T) {
	client, server := pair(t, session.Duplex)
	sent := make(chan error, 1)
	go func() { _, err := client.Write([]byte("first")); sent <- err }()
	select {
	case err := <-sent:
		t.Fatalf("write completed before peer consumed input: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	buf := make([]byte, 64)
	n, err := server.Read(buf)
	if err != nil || string(buf[:n]) != "first" {
		t.Fatalf("first input = %q, %v", buf[:n], err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	if err = client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err = server.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("half-close = %v", err)
	}
	go func() {
		_, err := server.Write([]byte("result after EOF"))
		if err == nil {
			err = server.Finish("")
		}
		sent <- err
	}()
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "result after EOF" {
		t.Fatalf("output = %q, %v", got, err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateDoesNotExecuteTwiceAndGapFails(t *testing.T) {
	client, server := pair(t, session.Duplex)
	f := session.Frame{Version: 1, ID: client.ID(), Op: "data", Seq: 1, Data: []byte("one")}
	if err := server.Receive(f); err != nil {
		t.Fatal(err)
	}
	if err := server.Receive(f); err != nil {
		t.Fatal(err)
	}
	// A second unacknowledged chunk is outside the one-chunk credit contract.
	f.Seq = 2
	if err := server.Receive(f); !errors.Is(err, session.ErrProtocol) {
		t.Fatalf("credit violation = %v", err)
	}
	_, other := pair(t, session.Duplex)
	f.ID = other.ID()
	if err := other.Receive(f); !errors.Is(err, session.ErrProtocol) {
		t.Fatalf("sequence gap = %v", err)
	}
}

func TestModeAndCancellationBoundBlockedWork(t *testing.T) {
	client, server := pair(t, session.ServerStream)
	if _, err := client.Write([]byte("unexpected input")); !errors.Is(err, session.ErrUnsupported) {
		t.Fatalf("one-way input = %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := server.Write([]byte("waiting")); done <- err }()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock write")
	}
}

func TestRequestReplyHasOneOutputDirectionAndEmptyResultCanClose(t *testing.T) {
	client, server := pair(t, session.RequestReply)
	done := make(chan error, 1)
	go func() { _, err := server.Write([]byte("answer")); done <- err }()
	buf := make([]byte, 64)
	if n, err := client.Read(buf); err != nil || string(buf[:n]) != "answer" {
		t.Fatalf("reply = %q, %v", buf[:n], err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("second request")); !errors.Is(err, session.ErrUnsupported) {
		t.Fatalf("additional request input = %v", err)
	}
	empty, provider := pair(t, session.RequestReply)
	if err := provider.Finish(""); err != nil {
		t.Fatal(err)
	}
	if err := empty.WaitReady(); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("empty reply = %v", err)
	}
}

func TestHalfCloseDoesNotHideTheFinalOutcome(t *testing.T) {
	client, server := pair(t, session.Duplex)
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(client); err != nil {
		t.Fatal(err)
	}
	final := make(chan error, 1)
	go func() { final <- client.Wait() }()
	select {
	case err := <-final:
		t.Fatalf("half-close became final outcome: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := server.Finish("transaction_rolled_back"); err != nil {
		t.Fatal(err)
	}
	if err := <-final; err == nil || err.Error() != "transaction_rolled_back" {
		t.Fatalf("final outcome = %v", err)
	}
}

func TestInitiatorTakesAnEarlyResultAsAcceptance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	open := func(mode session.Mode) *session.Stream {
		s, err := session.New(ctx, session.ID(), mode, true, func(context.Context, session.Frame) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// An empty reply sends accept and close with no ack between them.
	empty := open(session.RequestReply)
	if err := empty.Receive(session.Frame{Version: 1, ID: empty.ID(), Op: "close"}); err != nil {
		t.Fatal(err)
	}
	if err := empty.WaitReady(); err != nil {
		t.Fatalf("early close = %v", err)
	}
	if _, err := empty.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty reply = %v", err)
	}
	// A late accept still confirms the mode.
	duplex := open(session.Duplex)
	if err := duplex.Receive(session.Frame{Version: 1, ID: duplex.ID(), Op: "data", Seq: 1, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := duplex.Receive(session.Frame{Version: 1, ID: duplex.ID(), Op: "accept", Mode: session.ServerStream}); !errors.Is(err, session.ErrProtocol) {
		t.Fatalf("late accept with another mode = %v", err)
	}
}

func TestReadKeepsAChunkWhenItsAckFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	id := session.ID()
	var s *session.Stream
	s, err := session.New(ctx, id, session.ServerStream, true, func(ctx context.Context, f session.Frame) error {
		if f.Op != "ack" {
			return nil
		}
		// The peer finishes when the ack reaches it. Its close can arrive
		// before the transport confirms the ack, and cancels the send.
		if err := s.Receive(session.Frame{Version: 1, ID: id, Op: "close"}); err != nil {
			return err
		}
		return context.Cause(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []session.Frame{{Version: 1, ID: id, Op: "accept", Mode: session.ServerStream}, {Version: 1, ID: id, Op: "data", Seq: 1, Data: []byte("last")}} {
		if err = s.Receive(f); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := io.ReadAll(s); err != nil || string(got) != "last" {
		t.Fatalf("output = %q, %v", got, err)
	}
	if err = s.Wait(); err != nil {
		t.Fatal(err)
	}
}
