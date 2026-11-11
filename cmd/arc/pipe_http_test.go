package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

// httpSession runs arc session --http, and returns standard output and
// standard error.
func httpSession(t *testing.T, ctx context.Context, home string, args ...string) ([]byte, string, error) {
	t.Helper()
	cmd := root()
	cmd.SetArgs(append([]string{"--home", home, "session", "--http", "--mode", "server_stream", "--timeout", "90s"}, args...))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(ctx)
	return stdout.Bytes(), stderr.String(), err
}

// pattern is the body of /big of testdata/streaming-http: byte i is i mod
// 251.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestLargeBodyOverPipeThroughRelay gets 16 MiB from arc-http through a
// local relay. The body goes on the pipe, and arc names the path. The flag
// --path session and the citizen setting send the body in session frames.
func TestLargeBodyOverPipeThroughRelay(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	consumer, _ := providerHome(t, relay)
	web, webKey := providerHome(t, relay)
	serveSessionProvider(t, ctx, web, providerBinary(t, "../../apps/http/cmd/arc-http"), "testdata/streaming-http/interface.json", providerBinary(t, "./testdata/streaming-http"))
	ok(t, consumer, "", "install", webKey, "--yes")
	address := "http+arc://" + webKey + "/big"

	const size = 16 << 20
	body, notes, err := httpSession(t, ctx, consumer, address, `{"query":"bytes=16777216"}`)
	if err != nil {
		t.Fatalf("%v\n%s", err, notes)
	}
	if !bytes.Equal(body, pattern(size)) {
		t.Fatalf("got %d bytes, not the %d bytes of the body", len(body), size)
	}
	if !strings.Contains(notes, "path direct (host to host): 16777216 bytes in ") {
		t.Fatalf("the path line names no pipe: %q", notes)
	}

	body, notes, err = httpSession(t, ctx, consumer, "--path", "session", address, `{"query":"bytes=1048576"}`)
	if err != nil || !bytes.Equal(body, pattern(1<<20)) || !strings.Contains(notes, "path session (relay frames): 1048576 bytes in ") {
		t.Fatalf("--path session: %d bytes, %v, %q", len(body), err, notes)
	}

	ok(t, consumer, "", "path", "session")
	_, notes, err = httpSession(t, ctx, consumer, address, `{"query":"bytes=1024"}`)
	if err != nil || !strings.Contains(notes, "path session (relay frames): 1024 bytes in ") {
		t.Fatalf("arc path session: %v, %q", err, notes)
	}
}

// pipeProvider serves testdata/pipe-http in mode, and returns the address
// of /, and the file of the log of the provider.
func pipeProvider(t *testing.T, ctx context.Context, mode string) (consumer, address, log string) {
	t.Helper()
	relay := testrelay.Start(t)
	log = filepath.Join(t.TempDir(), "sessions")
	consumer, _ = providerHome(t, relay)
	home, key := providerHome(t, relay)
	program := programWith(t, providerBinary(t, "./testdata/pipe-http"), "PIPE_HTTP_MODE="+mode, "PIPE_HTTP_LOG="+log)
	serveSessionProvider(t, ctx, home, program, "testdata/streaming-http/interface.json")
	ok(t, consumer, "", "install", key, "--yes")
	return consumer, "http+arc://" + key + "/", log
}

func sessionsOf(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestBrokenPipeIsAnError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	consumer, address, log := pipeProvider(t, ctx, "break")

	body, notes, err := httpSession(t, ctx, consumer, address)
	if err == nil || !strings.Contains(err.Error(), "the pipe closed after") || !strings.Contains(err.Error(), "the outcome is not known") {
		t.Fatalf("%v, %d bytes, %q", err, len(body), notes)
	}
	if got := sessionsOf(t, log); got != "offer" {
		t.Fatalf("sessions %q, want one offer", got)
	}
}
