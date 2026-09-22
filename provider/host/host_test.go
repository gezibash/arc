package host_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gezibash/arc/provider/host"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A provider that ends when its input closes ends cleanly: it has time to
// release what it holds, and Stop reports no error.
func TestStopLetsTheProviderEnd(t *testing.T) {
	provider, err := host.Start("/bin/cat", nil, nil, quiet())
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
	provider, err := host.Start("/bin/sh", []string{"-c", "exec sleep 60"}, nil, quiet())
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
