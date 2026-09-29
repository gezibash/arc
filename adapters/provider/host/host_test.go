package host_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/adapters/provider/host"
	"github.com/gezibash/arc/core/provider/wire"
)

func TestBackpressureCannotOutliveTheSendDeadline(t *testing.T) {
	process, err := host.Start("/bin/sh", []string{"-c", "exec sleep 2"}, "", nil, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer process.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = process.Send(ctx, wire.Event{Op: "request", Message: wire.Text(strings.Repeat("x", 2*1024*1024))})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("send=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("Send waited for the non-reading process")
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A provider that ends when its input closes ends cleanly: it has time to
// release what it holds, and Stop reports no error.
func TestStopLetsTheProviderEnd(t *testing.T) {
	provider, err := host.Start("/bin/cat", nil, "", nil, quiet())
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if err := provider.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stop took %s", elapsed)
	}
}

// A provider that keeps running after its input closes is killed after the
// grace period.
func TestStopKillsAProviderThatDoesNotEnd(t *testing.T) {
	provider, err := host.Start("/bin/sh", []string{"-c", "exec sleep 60"}, "", nil, quiet())
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if err := provider.Stop(); err == nil {
		t.Fatal("a killed provider stopped without an error")
	}
	if elapsed := time.Since(started); elapsed < host.StopGrace || elapsed > host.StopGrace+2*time.Second {
		t.Fatalf("stop took %s, and the grace is %s", elapsed, host.StopGrace)
	}
}
