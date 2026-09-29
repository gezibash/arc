// Package testsession connects real core streams for provider behavior tests.
package testsession

import (
	"context"
	"github.com/gezibash/arc/core/session"
	"testing"
	"time"
)

func Start(t *testing.T, mode session.Mode, handler func(context.Context, *session.Stream) error) *session.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var client, server *session.Stream
	id := session.ID()
	client, _ = session.New(ctx, id, mode, true, func(_ context.Context, f session.Frame) error { return server.Receive(f) })
	server, _ = session.New(ctx, id, mode, false, func(_ context.Context, f session.Frame) error { return client.Receive(f) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Accept(); err != nil {
			return
		}
		err := handler(server.Context(), server)
		code := ""
		if err != nil {
			code = err.Error()
		}
		_ = server.Finish(code)
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("session handler did not stop")
		}
	})
	if err := client.WaitReady(); err != nil {
		t.Fatal(err)
	}
	return client
}
