package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

func TestCLISessionKeepsStateAndProducesOutputBeforeInputEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	url := testrelay.Start(t)
	providerHome, consumerHome := t.TempDir(), t.TempDir()
	for _, home := range []string{providerHome, consumerHome} {
		ok(t, home, "", "keys", "gen")
		ok(t, home, "", "relay", "add", url)
	}
	binary := filepath.Join(t.TempDir(), "repl")
	build := exec.Command("go", "build", "-o", binary, "../../examples/repl")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	manifest, err := filepath.Abs("../../examples/repl/interface.json")
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
	if !waitFor(serverOutput, "serves", 10*time.Second) {
		t.Fatalf("provider not ready: %s", serverOutput.String())
	}
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
