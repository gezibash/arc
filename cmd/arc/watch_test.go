package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gezibash/arc/adapters/transport/file"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/core/wire"
)

type idleProvider struct{ lines chan wire.Event }

func (p idleProvider) Send(context.Context, wire.Event) error { return nil }
func (p idleProvider) Lines() <-chan wire.Event               { return p.lines }

func TestServeDoesNotWaitForAnOfflineTransport(t *testing.T) {
	p := idleProvider{lines: make(chan wire.Event)}
	defer close(p.lines)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := call.NewServer(keys.Generate(), "test", p, 1024, nil, log)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dir := file.Dir{Path: t.TempDir()}
	missing, err := watchAll(ctx, s, []transport.Transport{dir}, log)
	if err != nil || len(missing) != 1 || missing[0] != dir.Name() {
		t.Fatalf("missing=%v err=%v", missing, err)
	}
}
