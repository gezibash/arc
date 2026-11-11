package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

// uploadSession runs arc session --http --mode duplex with input on
// standard input, and returns standard output and standard error.
func uploadSession(t *testing.T, ctx context.Context, home string, input io.Reader, args ...string) ([]byte, string, error) {
	t.Helper()
	cmd := root()
	cmd.SetArgs(append([]string{"--home", home, "session", "--http", "--mode", "duplex", "--timeout", "90s"}, args...))
	var stdout, stderr bytes.Buffer
	cmd.SetIn(input)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(ctx)
	return stdout.Bytes(), stderr.String(), err
}

// TestLargeUploadOverPipeThroughRelay sends 16 MiB to arc-http through a
// local relay. The request body goes on the pipe, the handler gets each
// byte, and arc names the path. With --path session, the body goes in
// session frames, and the handler gets each byte.
func TestLargeUploadOverPipeThroughRelay(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	consumer, _ := providerHome(t, relay)
	web, webKey := providerHome(t, relay)
	serveSessionProvider(t, ctx, web, providerBinary(t, "../../apps/http/cmd/arc-http"), "testdata/streaming-http/interface.json", providerBinary(t, "./testdata/streaming-http"))
	ok(t, consumer, "", "install", webKey, "--yes")
	address := "http+arc://" + webKey + "/upload"

	const size = 16 << 20
	body := pattern(size)
	want := fmt.Sprintf("%d %x", size, sha256.Sum256(body))
	got, notes, err := uploadSession(t, ctx, consumer, bytes.NewReader(body), address, `{"stream_body":true}`)
	if err != nil {
		t.Fatalf("%v\n%s", err, notes)
	}
	if string(got) != want {
		t.Fatalf("the handler got %q, want %q", got, want)
	}
	if !strings.Contains(notes, "upload path direct (host to host): 16777216 bytes in ") {
		t.Fatalf("the path line names no pipe: %q", notes)
	}

	// 16 MiB in frames takes 76 s with -race, so this run sends 1 MiB.
	small := body[:1<<20]
	want = fmt.Sprintf("%d %x", len(small), sha256.Sum256(small))
	got, notes, err = uploadSession(t, ctx, consumer, bytes.NewReader(small), "--path", "session", address, `{"stream_body":true}`)
	if err != nil || string(got) != want || !strings.Contains(notes, "upload path session (relay frames): 1048576 bytes in ") {
		t.Fatalf("--path session: %q, %v, %q", got, err, notes)
	}
}

// TestBrokenUploadPipeIsAnError uses a pipe that the provider closes after
// 64 KiB of the request body. arc stops with an error that says the
// outcome is not known, and does not ask again.
func TestBrokenUploadPipeIsAnError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	consumer, address, log := pipeProvider(t, ctx, "break")

	_, notes, err := uploadSession(t, ctx, consumer, bytes.NewReader(pattern(4<<20)), address, `{"stream_body":true}`)
	if err == nil || !strings.Contains(err.Error(), "of the request body; the outcome is not known") {
		t.Fatalf("%v, %q", err, notes)
	}
	if got := sessionsOf(t, log); got != "offer" {
		t.Fatalf("sessions %q, want one offer", got)
	}
}
