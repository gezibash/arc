package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/direct"
)

// TestAnAnswerOfAnotherVersionIsRefused gets an answer of version 2 from
// the sender. This end speaks version 1, so it refuses the answer and
// does not set an SDP that it may read wrong.
func TestAnAnswerOfAnotherVersionIsRefused(t *testing.T) {
	arc := filepath.Join(t.TempDir(), "arc")
	if err := os.WriteFile(arc, []byte("#!/bin/sh\nprintf '{\"version\":2}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := direct.Options{Loopback: true}
	pc, err := direct.NewConnection(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	if _, err := pc.CreateDataChannel("file", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := &getter{Arc: arcprogram.Arc{Program: arc}, sender: strings.Repeat("a", 64)}
	if err := g.signal(ctx, pc, options, 0); err == nil || !strings.Contains(err.Error(), "the sender gave no answer") {
		t.Fatalf("an answer of version 2 gave %v, want a refusal", err)
	}
}
