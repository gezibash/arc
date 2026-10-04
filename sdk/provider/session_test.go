package provider_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/sdk/provider"
)

type waitingSession struct{ entered, stopped chan struct{} }

func (h waitingSession) HandleRequest(context.Context, provider.Request) (string, error) {
	return "ordinary", nil
}
func (h waitingSession) HandleSession(ctx context.Context, _ provider.Request, _ *session.Stream) error {
	close(h.entered)
	<-ctx.Done()
	close(h.stopped)
	return ctx.Err()
}
func frameOf(t *testing.T, reply map[string]any) session.Frame {
	t.Helper()
	body, err := json.Marshal(reply["session"])
	if err != nil {
		t.Fatal(err)
	}
	var frame session.Frame
	if err = json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}
func TestSessionSharesAdmissionAndCancellationBypassesIt(t *testing.T) {
	h := waitingSession{make(chan struct{}), make(chan struct{})}
	p := startPipesWithOptions(t, context.Background(), h, provider.Options{MaxConcurrent: 1})
	id := session.ID()
	p.send(t, map[string]any{"op": "session", "request_id": id, "message": "", "meta": map[string]any{}, "session": session.Frame{Version: 1, ID: id, Op: "open", Mode: session.Duplex}})
	if f := frameOf(t, p.read(t)); f.Op != "accept" {
		t.Fatalf("open = %+v", f)
	}
	<-h.entered
	p.request(t, "overflow", "must not run while session is active")
	if reply := p.read(t); reply["error"] != "provider_busy" {
		t.Fatalf("admission = %v", reply)
	}
	p.send(t, map[string]any{"op": "session", "request_id": id, "session": session.Frame{Version: 1, ID: id, Op: "cancel"}})
	select {
	case <-h.stopped:
	case <-time.After(time.Second):
		t.Fatal("session cancellation waited behind admission")
	}
	if f := frameOf(t, p.read(t)); f.Op != "close" || f.Error != "provider_canceled" {
		t.Fatalf("cancellation = %+v", f)
	}
}
func TestMalformedFrameStopsItsSession(t *testing.T) {
	h := waitingSession{make(chan struct{}), make(chan struct{})}
	p := startPipes(t, h)
	id := session.ID()
	p.send(t, map[string]any{"op": "session", "request_id": id, "message": "", "meta": map[string]any{}, "session": session.Frame{Version: 1, ID: id, Op: "open", Mode: session.Duplex}})
	_ = p.read(t)
	<-h.entered
	p.send(t, map[string]any{"op": "session", "request_id": id, "session": session.Frame{Version: 99, ID: id, Op: "data", Seq: 1, Data: []byte("bad")}})
	if f := frameOf(t, p.read(t)); f.Op != "close" || f.Error != "session_protocol_error" {
		t.Fatalf("malformed frame = %+v", f)
	}
}
