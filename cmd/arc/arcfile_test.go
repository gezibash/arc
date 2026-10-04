package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

// serveDir runs arc serve <dir> with the flags, and waits until it serves.
func serveDir(t *testing.T, ctx context.Context, home, dir string, flags ...string) *output {
	t.Helper()
	cmd := root()
	cmd.SetArgs(append([]string{"--home", home, "serve", dir}, flags...))
	out := &output{}
	cmd.SetOut(out)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the provider failed to stop")
		}
	})
	if !waitFor(out, "serves", 10*time.Second) {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("serve %s stopped: %v", dir, err)
		default:
			t.Fatalf("provider not ready: %s", out.String())
		}
	}
	return out
}

// app writes an app directory: an Arcfile and an interface manifest.
func app(t *testing.T, id, arcfile string, interactions ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	modes := `"request_reply"`
	for _, mode := range interactions {
		modes += `, "` + mode + `"`
	}
	manifest := fmt.Sprintf(`{"interface": 1, "id": %q, "shape": "service", "title": %q, "summary": "A test app.",
  "service": {"method": "ECHO", "path": "/", "max_bytes": 65536, "interactions": [%s]}, "kinds": {},
  "commands": [{"path": ["say"], "summary": "Send text",
    "args": [{"name": "text", "kind": "positional", "type": "text", "variadic": true, "required": true}],
    "action": {"call": {"class": "live", "body": "{{text|join}}"}}}]}`, id, id, modes)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Arcfile"), []byte(arcfile), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// An Arcfile of version 2 limits who reaches the program, and what the
// program calls, through a real relay and the normal CLI.
func TestArcfileAllowAndUsesThroughCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	echo := providerBinary(t, "../../adapters/provider/host/testdata/echo")
	stdio := fmt.Sprintf("version = 2\n[serve]\ncommand = %q\nmanifest = \"./manifest.json\"\n", echo)

	// Two backend apps. The operator installs both, and uses only alpha.
	alphaHome, alphaKey := providerHome(t, relay)
	betaHome, betaKey := providerHome(t, relay)
	serveDir(t, ctx, alphaHome, app(t, "alpha", stdio))
	serveDir(t, ctx, betaHome, app(t, "beta", stdio))
	operator, operatorKey := providerHome(t, relay)
	ok(t, operator, "", "install", alphaKey, "--yes")
	ok(t, operator, "", "install", betaKey, "--yes")

	caller, callerKey := providerHome(t, relay)
	stranger, _ := providerHome(t, relay)
	front := app(t, "front", stdio+fmt.Sprintf("allow = [%q]\n[uses]\na = \"alpha\"\n", callerKey), "duplex")
	serveDir(t, ctx, operator, front)
	ok(t, caller, "", "install", operatorKey, "--yes")
	ok(t, stranger, "", "install", operatorKey, "--yes")
	address := "front+arc://" + operatorKey + "/"

	if out := ok(t, caller, "", "call", address, "hello", "--raw"); !strings.Contains(out, "hello") {
		t.Fatalf("an allowed caller: %q", out)
	}
	if out, err := run(t, stranger, "", "call", address, "hello", "--raw"); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("a caller that the Arcfile does not allow: %v %q", err, out)
	}
	if out, err := run(t, stranger, "", "session", "--timeout", "10s", address, "hello"); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("a session of a caller that the Arcfile does not allow: %v %q", err, out)
	}

	want := "alpha+arc://" + alphaKey + "/"
	if out := ok(t, caller, "", "call", address, "env ARC_USE_A", "--raw"); strings.TrimSpace(out) != want {
		t.Fatalf("ARC_USE_A = %q, want %q", out, want)
	}
	if out := ok(t, caller, "", "call", address, "call "+want+" hi", "--raw"); !strings.Contains(out, "reply: ECHO / hi") {
		t.Fatalf("a call to an app in [uses]: %q", out)
	}
	if out := ok(t, caller, "", "call", address, "call beta+arc://"+betaKey+"/ hi", "--raw"); !strings.Contains(out, "not_in_uses") {
		t.Fatalf("a call to an installed app outside [uses]: %q", out)
	}

	// [uses] that names an app the operator did not install stops the start.
	lonely, _ := providerHome(t, relay)
	cmd := root()
	cmd.SetArgs([]string{"--home", lonely, "serve", app(t, "lonely", stdio+"[uses]\na = \"alpha\"\n")})
	if err := cmd.ExecuteContext(ctx); err == nil || !strings.Contains(err.Error(), "has not installed") {
		t.Fatalf("serve with an app in [uses] that is not installed: %v", err)
	}
}

// protocol = "http" runs arc-http, which runs an HTTP server of any
// language. The operator writes no exec:// address.
func TestArcfileProtocolHTTPThroughCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	relay := testrelay.Start(t)

	bin := t.TempDir()
	translator := providerBinary(t, "../../apps/http/cmd/arc-http")
	if err := os.Rename(translator, filepath.Join(bin, "arc-http")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	manifest, err := filepath.Abs("testdata/streaming-http/interface.json")
	if err != nil {
		t.Fatal(err)
	}
	server := providerBinary(t, "./testdata/streaming-http")
	dir := filepath.Join(t.TempDir(), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	arcfile := fmt.Sprintf("version = 2\n[serve]\ncommand = %q\nprotocol = \"http\"\nmanifest = %q\n", server, manifest)
	if err := os.WriteFile(filepath.Join(dir, "Arcfile"), []byte(arcfile), 0o644); err != nil {
		t.Fatal(err)
	}

	webHome, webKey := providerHome(t, relay)
	serveDir(t, ctx, webHome, dir)
	caller, _ := providerHome(t, relay)
	ok(t, caller, "", "install", webKey, "--yes")
	_, out, done := startSessionCLI(t, ctx, caller, "--http", "--mode", "server_stream", "http+arc://"+webKey+"/events")
	sessionDone(t, done, 0)
	if !strings.Contains(out.String(), "data: first") || !strings.Contains(out.String(), "data: second") {
		t.Fatalf("SSE through protocol http: %s", out.String())
	}
}
