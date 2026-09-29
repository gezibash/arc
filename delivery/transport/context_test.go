package transport_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
	"github.com/gezibash/arc/delivery/transport/relay"
)

// Every adapter must refuse work whose caller has already stopped. Keep this
// matrix shared so adding a transport exercises the same contract.
func TestTransportContextContract(t *testing.T) {
	factories := map[string]func(*testing.T) transport.Transport{
		"directory": func(t *testing.T) transport.Transport { return file.Dir{Path: t.TempDir()} },
		"relay":     func(t *testing.T) transport.Transport { return relay.Relay{URL: testrelay.Start(t)} },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			for _, expired := range []bool{false, true} {
				label := "canceled"
				if expired {
					label = "expired"
				}
				t.Run(label, func(t *testing.T) {
					adapter := factory(t)
					ctx, cancel := context.WithCancel(context.Background())
					if expired {
						cancel()
						ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
					}
					cancel()
					event := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "must not be sent", Tags: nostr.Tags{}}
					if err := event.Sign(keys.Generate().Secret); err != nil {
						t.Fatal(err)
					}
					operations := map[string]func() error{
						"send":  func() error { return adapter.Send(ctx, event) },
						"fetch": func() error { _, err := adapter.Fetch(ctx, nostr.Filter{}); return err },
					}
					if carrier, ok := adapter.(transport.Carrier); ok {
						operations["send_hops"] = func() error { return carrier.SendHops(ctx, event, 3) }
					}
					if live, ok := adapter.(transport.Live); ok {
						operations["watch"] = func() error { _, err := live.Watch(ctx, nostr.Filter{}); return err }
					}
					if reconciler, ok := adapter.(transport.Reconciler); ok {
						for _, kind := range []nostr.Kind{1, 31234} {
							operations["reconcile_"+kind.String()] = func() error {
								_, _, _, err := reconciler.Reconcile(ctx, nostr.Filter{Kinds: []nostr.Kind{kind}}, nil)
								return err
							}
						}
					}
					for op, run := range operations {
						t.Run(op, func(t *testing.T) {
							if err := run(); !errors.Is(err, ctx.Err()) {
								t.Fatalf("error = %v, want %v", err, ctx.Err())
							}
						})
					}
					batch, err := adapter.Fetch(context.Background(), nostr.Filter{IDs: []nostr.ID{event.ID}})
					if err != nil || len(batch.Events) != 0 {
						t.Fatalf("canceled send left %d events: %v", len(batch.Events), err)
					}
				})
			}
		})
	}
}
