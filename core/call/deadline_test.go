package call_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/provider/wire"
)

type protocolClient struct{ sent, replies chan wire.Event }

func (p *protocolClient) Send(ctx context.Context, event wire.Event) error {
	select {
	case p.sent <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *protocolClient) Lines() <-chan wire.Event { return p.replies }

func TestServerPropagatesDeadlineAndCancellation(t *testing.T) {
	client := &protocolClient{sent: make(chan wire.Event, 2), replies: make(chan wire.Event)}
	defer close(client.replies)
	key := keys.Generate()
	server := call.NewServer(key, "counter", client, 1024, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	rumor := call.RequestRumor(keys.Generate(), key.Public, call.Request{Capability: "counter", Body: "increment"}, time.Now())
	finished := make(chan call.Reply, 1)
	go func() {
		reply, err := server.Handle(ctx, rumor)
		if err != nil {
			t.Error(err)
		}
		finished <- reply
	}()
	request := <-client.sent
	if request.Op != "request" || request.DeadlineMS != deadline.UnixMilli() {
		t.Fatalf("request=%+v", request)
	}
	select {
	case stop := <-client.sent:
		if stop.Op != "cancel" || stop.RequestID != request.RequestID {
			t.Fatalf("cancel=%+v", stop)
		}
	case <-time.After(time.Second):
		t.Fatal("no cancellation reached the provider")
	}
	if reply := <-finished; !strings.HasPrefix(reply.Err, "provider_timeout") {
		t.Fatalf("reply=%+v", reply)
	}
}

func TestServerCancelsAnOutboundCall(t *testing.T) {
	client := &protocolClient{sent: make(chan wire.Event, 2), replies: make(chan wire.Event)}
	defer close(client.replies)
	entered, canceled := make(chan struct{}), make(chan struct{})
	call.NewServer(keys.Generate(), "parent", client, 1024, func(ctx context.Context, _ call.Outbound) (call.Reply, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return call.Reply{}, ctx.Err()
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.replies <- wire.Event{Op: "call", CallID: "child", Address: "x+arc://provider/", Body: wire.Text("work")}
	<-entered
	client.replies <- wire.Event{Op: "cancel", CallID: "child"}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("outbound call survived cancellation")
	}
}
