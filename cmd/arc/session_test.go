package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

func TestCLISessionKeepsStateAndProducesOutputBeforeInputEOF(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	url := testrelay.Start(t)
	providerHome, consumerHome := t.TempDir(), t.TempDir()
	for _, home := range []string{providerHome, consumerHome} {
		ok(t, home, "", "keys", "gen")
		ok(t, home, "", "relay", "add", url)
	}
	binary := providerBinary(t, "./testdata/repl")
	manifest, err := filepath.Abs("testdata/repl/interface.json")
	if err != nil {
		t.Fatal(err)
	}
	serving := root()
	serving.SetArgs([]string{"--home", providerHome, "serve", "exec://" + binary + "?manifest=" + manifest})
	serverOutput := &output{}
	serving.SetOut(serverOutput)
	serverDone := make(chan error, 1)
	go func() { serverDone <- serving.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			t.Error("provider did not stop")
		}
	}()
	waitServes(t, serverOutput, serverDone, 10*time.Second)
	lines := strings.Split(strings.TrimSpace(serverOutput.String()), "\n")
	public := strings.TrimSpace(lines[len(lines)-1])
	ok(t, consumerHome, "", "install", public, "repl", "--yes")
	address := "repl+arc://" + public + "/"
	input, writer := io.Pipe()
	defer writer.Close()
	command := root()
	command.SetArgs([]string{"--home", consumerHome, "session", address, "--timeout", "10s"})
	command.SetIn(input)
	out := &output{}
	command.SetOut(out)
	done := make(chan error, 1)
	go func() { done <- command.ExecuteContext(ctx) }()
	if !waitFor(out, "ready\n", 5*time.Second) {
		t.Fatalf("no output before input EOF: %s", out.String())
	}
	if _, err = io.WriteString(writer, "SET answer 42\n"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(out, "ok\n", 3*time.Second) {
		t.Fatalf("SET did not answer: %s", out.String())
	}
	if _, err = io.WriteString(writer, "GET answer\n"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(out, "42\n", 3*time.Second) {
		t.Fatalf("session lost state: %s", out.String())
	}
	writer.Close()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("half-close did not finish")
	}
	second := root()
	second.SetArgs([]string{"--home", consumerHome, "session", address, "--timeout", "5s"})
	second.SetIn(strings.NewReader("GET answer\n"))
	isolated := &output{}
	second.SetOut(isolated)
	if err = second.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if isolated.String() != "ready\n\n" {
		t.Fatalf("sessions shared application state: %q", isolated.String())
	}
}

// arc session takes one output mapping, and one that the mode allows. It
// refuses the others before it opens a session.
func TestASessionRefusesAMappingThatDoesNotFit(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	for _, c := range []struct {
		flags []string
		want  string
	}{
		{[]string{"--exec", "--http"}, "choose one session output mapping"},
		{[]string{"--tty", "--websocket"}, "choose one session output mapping"},
		{[]string{"--http", "--mode", "request_reply"}, "HTTP streaming needs server_stream or duplex"},
		{[]string{"--websocket", "--mode", "server_stream"}, "WebSockets need duplex mode"},
		{[]string{"--exec", "--mode", "request_reply"}, "exec I/O needs server_stream or duplex"},
	} {
		_, err := run(t, home, "", append([]string{"session", "exec+arc://" + strings.Repeat("a", 64) + "/"}, c.flags...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want an error with %q", c.flags, err, c.want)
		}
	}
}
