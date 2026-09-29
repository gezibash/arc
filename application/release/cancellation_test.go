package release_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gezibash/arc/application/release"
)

func TestCanceledProbeKeepsTheCurrentProgram(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arc")
	old := "the original executable"
	if err := os.WriteFile(path, []byte(old), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := release.Replace(ctx, path, []byte("#!/bin/sh\nexec sleep 2\n"), "never")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replace=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("the probe ignored cancellation")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != old {
		t.Fatalf("original=%q err=%v", got, err)
	}
}

func TestCheckpointCannotMoveBackwardsAfterVerification(t *testing.T) {
	c := &release.Checkpoint{Dir: t.TempDir()}
	publisher := []byte("test publisher")
	if err := c.Write(publisher, &release.Channel{Name: "stable", Sequence: 8, Digest: "eight"}); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []*release.Channel{
		{Name: "stable", Sequence: 7, Digest: "seven"},
		{Name: "stable", Sequence: 8, Digest: "another-eight"},
	} {
		if err := c.Write(publisher, ch); !errors.Is(err, release.ErrOutOfSequence) {
			t.Fatalf("stale write=%v", err)
		}
	}
	got, err := c.Read(publisher, "stable")
	if err != nil || got.LastSequence != 8 || got.LastDigest != "eight" {
		t.Fatalf("checkpoint=%+v err=%v", got, err)
	}
}
