package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

// oldProgram is the program that arc update replaces in these tests.
const oldProgram = "#!/bin/sh\necho arc 0.9.0\n"

// releaseRoot writes a releases root for the provider in home: an archive
// that holds the program arc, which reports 9.9.9, and 4 MiB of random
// bytes, and a channel that the provider signs. It returns the root and
// the size of the archive.
func releaseRoot(t *testing.T, home, key string) (string, int) {
	t.Helper()
	root := t.TempDir()
	padding := make([]byte, 4<<20)
	_, _ = rand.Read(padding)
	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	packed := tar.NewWriter(zipped)
	for _, member := range []struct {
		name string
		body []byte
	}{{"arc/bin/arc", []byte("#!/bin/sh\necho arc 9.9.9\n")}, {"arc/padding", padding}} {
		if err := packed.WriteHeader(&tar.Header{Name: member.name, Mode: 0o755, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := packed.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if packed.Close() != nil || zipped.Close() != nil {
		t.Fatal("the archive did not close")
	}
	sum := sha256.Sum256(archive.Bytes())
	digest := hex.EncodeToString(sum[:])
	for _, dir := range []string{"blobs", "channels"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", digest+".tar.gz"), archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	unsigned := filepath.Join(t.TempDir(), "unsigned.json")
	channel := fmt.Sprintf(`{"schema_version": 3, "channel": "stable", "publisher": %q, "sequence": 1, "expires_at": %d,
 "releases": [{"version": "9.9.9", "build": "9.9.9+test", "runtime": "go", "platform": {"os": %q, "arch": %q},
   "size": %d, "sha256": %q, "sources": [], "restart_required": true, "withdrawn": false, "eligible": true,
   "install": {"size": %d, "sha256": %q}}]}`,
		key, time.Now().Unix()+86400, runtime.GOOS, runtime.GOARCH, archive.Len(), digest, archive.Len(), digest)
	if err := os.WriteFile(unsigned, []byte(channel), 0o600); err != nil {
		t.Fatal(err)
	}
	ok(t, home, "", "release", "sign", "--root", root, unsigned)
	return root, archive.Len()
}

// releasesProvider serves binary as the releases provider of a new
// citizen on relay. It returns the key of the provider and the size of the
// archive.
func releasesProvider(t *testing.T, ctx context.Context, relay, binary string) (string, int) {
	t.Helper()
	home, key := providerHome(t, relay)
	root, size := releaseRoot(t, home, key)
	t.Setenv("RELEASES_ROOT", root)
	serveSessionProvider(t, ctx, home, binary, "../../apps/releases/manifest.json")
	return key, size
}

// updateApply runs arc update apply on a new copy of oldProgram. It
// returns the program after the run, standard error and the error.
func updateApply(t *testing.T, ctx context.Context, home, key string, args ...string) (string, string, error) {
	t.Helper()
	program := filepath.Join(t.TempDir(), "arc")
	if err := os.WriteFile(program, []byte(oldProgram), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := root()
	cmd.SetArgs(append([]string{"--home", home, "update", "apply", "--provider", key, "--program", program}, args...))
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(ctx)
	after, readErr := os.ReadFile(program)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(after), stderr.String(), err
}

// TestArchiveOverPipeThroughRelay updates from the releases app through a
// local relay. The archive goes on the pipe, and arc names the path. With
// --path session, and for a citizen that did not install the app, the
// archive comes in chunks.
func TestArchiveOverPipeThroughRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	pipesOnLoopback(t)
	key, size := releasesProvider(t, ctx, relay, providerBinary(t, "../../apps/releases/cmd/arc-releases"))
	consumer, _ := providerHome(t, relay)
	ok(t, consumer, "", "install", key, "--yes")

	program, notes, err := updateApply(t, ctx, consumer, key)
	if err != nil {
		t.Fatalf("%v\n%s", err, notes)
	}
	if !strings.Contains(program, "9.9.9") {
		t.Fatalf("the program is %q", program)
	}
	if !strings.Contains(notes, fmt.Sprintf("path direct (host to host): %d bytes in ", size)) {
		t.Fatalf("the path line names no pipe: %q", notes)
	}

	program, notes, err = updateApply(t, ctx, consumer, key, "--path", "session")
	if err != nil || !strings.Contains(program, "9.9.9") || !strings.Contains(notes, fmt.Sprintf("path chunks (relay replies): %d bytes in ", size)) {
		t.Fatalf("--path session: %v, %q", err, notes)
	}

	stranger, _ := providerHome(t, relay)
	program, notes, err = updateApply(t, ctx, stranger, key)
	if err != nil || !strings.Contains(program, "9.9.9") || !strings.Contains(notes, "is not installed with server_stream, so arc opens no pipe; using chunks over the relay") ||
		!strings.Contains(notes, "path chunks (relay replies)") {
		t.Fatalf("not installed: %v, %q", err, notes)
	}
	program, notes, err = updateApply(t, ctx, stranger, key, "--path", "direct")
	if err == nil || !strings.Contains(err.Error(), "or run again with --path session") || program != oldProgram {
		t.Fatalf("not installed, --path direct: %v, %q", err, notes)
	}
}

// pipeReleases serves testdata/pipe-releases in mode to a citizen that
// installed it. It returns the consumer, the key of the provider and the
// file of the log of the provider.
func pipeReleases(t *testing.T, ctx context.Context, mode string) (consumer, key, log string) {
	t.Helper()
	relay := testrelay.Start(t)
	pipesOnLoopback(t)
	log = filepath.Join(t.TempDir(), "sessions")
	t.Setenv("PIPE_RELEASES_MODE", mode)
	t.Setenv("PIPE_RELEASES_LOG", log)
	key, _ = releasesProvider(t, ctx, relay, providerBinary(t, "./testdata/pipe-releases"))
	consumer, _ = providerHome(t, relay)
	ok(t, consumer, "", "install", key, "--yes")
	return consumer, key, log
}

// TestOlderReleasesProviderFallsBackToChunks updates from a provider that
// refuses the field "direct" as invalid_request, as a provider before the
// pipe does. In auto, arc says so and reads the archive in chunks. With
// --path direct, arc stops.
func TestOlderReleasesProviderFallsBackToChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	consumer, key, log := pipeReleases(t, ctx, "old")

	program, notes, err := updateApply(t, ctx, consumer, key)
	if err != nil || !strings.Contains(program, "9.9.9") {
		t.Fatalf("%v, %q", err, notes)
	}
	if !strings.Contains(notes, "the provider has no pipe; using chunks over the relay") || !strings.Contains(notes, "path chunks (relay replies)") {
		t.Fatalf("%q", notes)
	}
	if got := sessionsOf(t, log); got != "offer\nchunks" {
		t.Fatalf("sessions %q, want an offer and then chunks", got)
	}

	program, notes, err = updateApply(t, ctx, consumer, key, "--path", "direct")
	if err == nil || !strings.Contains(err.Error(), "the provider has no pipe; run again with --path session") || program != oldProgram {
		t.Fatalf("--path direct: %v, %q", err, notes)
	}
	if got := sessionsOf(t, log); got != "offer\nchunks\noffer" {
		t.Fatalf("sessions %q after --path direct", got)
	}
}

// TestArchivePipeFailureIsAnError updates from a provider that answers
// each offer, but no pipe opens. arc makes the two direct attempts and the
// relay attempt, and stops. It never reads chunks, and the program stays.
func TestArchivePipeFailureIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	consumer, key, log := pipeReleases(t, ctx, "deaf")

	program, notes, err := updateApply(t, ctx, consumer, key)
	if err == nil || !strings.Contains(err.Error(), "no direct path, and no path through the relay") || program != oldProgram {
		t.Fatalf("%v, %q", err, notes)
	}
	if got := sessionsOf(t, log); got != "offer\noffer" {
		t.Fatalf("sessions %q, want two offers and no chunks", got)
	}
}

// TestBrokenArchivePipeIsAnError updates from a provider whose pipe closes
// after 64 KiB of the archive.
func TestBrokenArchivePipeIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	consumer, key, log := pipeReleases(t, ctx, "break")

	program, notes, err := updateApply(t, ctx, consumer, key)
	if err == nil || !strings.Contains(err.Error(), "the pipe closed after") || !strings.Contains(err.Error(), "the outcome is not known") || program != oldProgram {
		t.Fatalf("%v, %q", err, notes)
	}
	if got := sessionsOf(t, log); got != "offer" {
		t.Fatalf("sessions %q, want one offer", got)
	}
}

// TestBadArchiveOverPipeIsRefused updates from a provider that sends the
// archive with one byte changed, and the right count. arc checks the hash
// as it does for chunks, and keeps the program.
func TestBadArchiveOverPipeIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	consumer, key, _ := pipeReleases(t, ctx, "bad")

	program, notes, err := updateApply(t, ctx, consumer, key)
	if err == nil || !strings.Contains(err.Error(), "does not match its size and hash") || program != oldProgram {
		t.Fatalf("%v, %q", err, notes)
	}
}
