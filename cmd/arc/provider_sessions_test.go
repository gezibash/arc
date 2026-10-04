package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/runtime/iface"
	"golang.org/x/term"
)

func providerBinary(t *testing.T, pkg string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "program")
	cmd := exec.Command("go", "build", "-o", binary, pkg)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return binary
}
func providerHome(t *testing.T, relay string) (string, string) {
	t.Helper()
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	ok(t, home, "", "relay", "add", relay)
	return home, strings.Split(ok(t, home, "", "whoami"), "\n")[1]
}
func serveSessionProvider(t *testing.T, ctx context.Context, home, binary, manifest string, args ...string) {
	t.Helper()
	manifest, err := filepath.Abs(manifest)
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{"manifest": {manifest}}
	for _, arg := range args {
		query.Add("args", arg)
	}
	cmd := root()
	cmd.SetArgs([]string{"--home", home, "serve", "exec://" + binary + "?" + query.Encode()})
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
			t.Error("provider failed to stop")
		}
	})
	if !waitFor(out, "serves", 10*time.Second) {
		t.Fatalf("provider not ready: %s", out.String())
	}
}
func startSessionCLI(t *testing.T, ctx context.Context, home string, args ...string) (*io.PipeWriter, *output, <-chan error) {
	t.Helper()
	input, writer := io.Pipe()
	t.Cleanup(func() { writer.Close(); input.Close() })
	cmd := root()
	cmd.SetArgs(append([]string{"--home", home, "session", "--timeout", "15s"}, args...))
	cmd.SetIn(input)
	out := &output{}
	cmd.SetOut(out)
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	return writer, out, done
}
func sessionDone(t *testing.T, done <-chan error, wantExit int) {
	t.Helper()
	select {
	case err := <-done:
		if wantExit == 0 && err != nil {
			t.Fatal(err)
		}
		if wantExit != 0 {
			var exit iface.ExitError
			if !errors.As(err, &exit) || exit.Code != wantExit {
				t.Fatalf("exit: %v", err)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not finish")
	}
}
func TestBundledProviderSessionsThroughCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	consumer, key := providerHome(t, relay)
	sqlHome, sqlKey := providerHome(t, relay)
	httpHome, httpKey := providerHome(t, relay)
	sqlConfig := filepath.Join(t.TempDir(), "sqlite.json")
	data, _ := json.Marshal(map[string]any{"databases": map[string]any{"main": map[string]any{"path": filepath.Join(t.TempDir(), "main.db"), "grants": map[string]string{key: "write", httpKey: "read"}}}})
	if err := os.WriteFile(sqlConfig, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQLITE_CONFIG", sqlConfig)
	serveSessionProvider(t, ctx, sqlHome, providerBinary(t, "../../apps/sqlite/cmd/arc-sqlite"), "../../apps/sqlite/manifest.json")
	ok(t, consumer, "", "install", sqlKey, "--yes")
	input, out, done := startSessionCLI(t, ctx, consumer, "sqlite+arc://"+sqlKey+"/main")
	if !waitFor(out, `"type":"ready"`, 5*time.Second) {
		t.Fatal(out.String())
	}
	io.WriteString(input, "CREATE TEMP TABLE local(n);\nINSERT INTO local VALUES(42);\nSELECT n FROM local;\n")
	if !waitFor(out, `"row":[42]`, 5*time.Second) {
		t.Fatalf("SQL lost state: %s", out.String())
	}
	input.Close()
	sessionDone(t, done, 0)
	// A second participant consumes SQLite through the hosted HTTP session API.
	ok(t, httpHome, "", "install", sqlKey, "--yes")
	serveSessionProvider(t, ctx, httpHome, providerBinary(t, "../../apps/http/cmd/arc-http"), "testdata/streaming-http/interface.json", providerBinary(t, "./testdata/streaming-http"))
	ok(t, consumer, "", "install", httpKey, "--yes")
	input, out, done = startSessionCLI(t, ctx, consumer, "--http", "--mode", "server_stream", "http+arc://"+httpKey+"/events")
	if !waitFor(out, "data: first", 5*time.Second) {
		t.Fatalf("SSE did not stream: %s", out.String())
	}
	sessionDone(t, done, 0)
	if !strings.Contains(out.String(), "data: second") {
		t.Fatal(out.String())
	}
	input.Close()
	input, out, done = startSessionCLI(t, ctx, consumer, "--websocket", "http+arc://"+httpKey+"/socket")
	if !waitFor(out, "ready", 5*time.Second) {
		t.Fatal(out.String())
	}
	io.WriteString(input, "echo over ARC\n")
	if !waitFor(out, "echo over ARC", 5*time.Second) {
		t.Fatal(out.String())
	}
	input.Close()
	sessionDone(t, done, 0)
	nested, _ := json.Marshal(map[string]string{"query": url.Values{"address": {"sqlite+arc://" + sqlKey + "/main"}, "body": {"SELECT 99"}}.Encode()})
	input, out, done = startSessionCLI(t, ctx, consumer, "--http", "--mode", "server_stream", "http+arc://"+httpKey+"/nested", string(nested))
	sessionDone(t, done, 0)
	input.Close()
	if !strings.Contains(out.String(), `"row":[99]`) {
		t.Fatalf("nested SQL: %s", out.String())
	}
	execHome, execKey := providerHome(t, relay)
	config := filepath.Join(t.TempDir(), "exec.json")
	data, _ = json.Marshal(map[string]any{"grants": []string{key}, "cwd": t.TempDir()})
	os.WriteFile(config, data, 0600)
	t.Setenv("EXEC_CONFIG", config)
	serveSessionProvider(t, ctx, execHome, providerBinary(t, "../../apps/exec/cmd/arc-exec"), "../../apps/exec/manifest.json")
	ok(t, consumer, "", "install", execKey, "--yes")
	input, out, done = startSessionCLI(t, ctx, consumer, "--exec", "exec+arc://"+execKey+"/", `{"script":"printf ready; read word; printf '<%s>' \"$word\"; exit 7"}`)
	if !waitFor(out, "ready", 5*time.Second) {
		t.Fatal(out.String())
	}
	io.WriteString(input, "interactive\n")
	sessionDone(t, done, 7)
	input.Close()
	if !strings.Contains(out.String(), "<interactive>") {
		t.Fatal(out.String())
	}
	// The actual CLI runs under a terminal and forwards its size and raw input.
	arc := providerBinary(t, ".")
	command := exec.CommandContext(ctx, "sh", "-c", `"$@"; result=$?; printf "\ntty-finished\n"; read stop; exit "$result"`, "sh", arc, "--home", consumer, "session", "--tty", "--timeout", "10s", "exec+arc://"+execKey+"/", `{"script":"stty -echo; printf tty-ready; read word; printf 'tty-%s' \"$word\""}`)
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer slave.Close()
	if err = pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	original, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	command.Stdin = slave
	command.Stdout = slave
	command.Stderr = slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}

	terminalOut := &output{}
	copied := make(chan struct{})
	go func() { io.Copy(terminalOut, terminal); close(copied) }()
	if !waitFor(terminalOut, "tty-ready", 5*time.Second) {
		t.Fatalf("CLI PTY: %s", terminalOut.String())
	}
	io.WriteString(terminal, "answer\n")
	if !waitFor(terminalOut, "tty-finished", 5*time.Second) {
		t.Fatalf("CLI did not restore terminal: %s", terminalOut.String())
	}
	restored, err := term.GetState(int(slave.Fd()))
	if err != nil || *restored != *original {
		t.Fatalf("terminal was not restored: %v", err)
	}
	io.WriteString(terminal, "done\n")
	if err = command.Wait(); err != nil {
		slave.Close()
		terminal.Close()
		<-copied
		t.Fatalf("%v: %s", err, terminalOut.String())
	}

	slave.Close()
	terminal.Close()
	<-copied
	if !strings.Contains(terminalOut.String(), "tty-answer") {
		t.Fatal(terminalOut.String())
	}
	relHome, relKey := providerHome(t, relay)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "blobs"), 0700)
	archive := strings.Repeat("archive", 10000)
	digest := sha256.Sum256([]byte(archive))
	hexDigest := hex.EncodeToString(digest[:])
	os.WriteFile(filepath.Join(root, "blobs", hexDigest+".tar.gz"), []byte(archive), 0600)
	t.Setenv("RELEASES_ROOT", root)
	serveSessionProvider(t, ctx, relHome, providerBinary(t, "../../apps/releases/cmd/arc-releases"), "../../apps/releases/manifest.json")
	ok(t, consumer, "", "install", relKey, "--yes")
	input, out, done = startSessionCLI(t, ctx, consumer, "--mode", "server_stream", "releases+arc://"+relKey+"/releases", `{"op":"archive","digest":"sha256:`+hexDigest+`"}`)
	sessionDone(t, done, 0)
	input.Close()
	if out.String() != archive {
		t.Fatalf("archive bytes: %d", len(out.String()))
	}
}

// ttyRun is the arc program under a terminal, as a person runs it.
type ttyRun struct {
	command  *exec.Cmd
	terminal *os.File
	slave    *os.File
	original *term.State
	out      *output
}

func startTTY(t *testing.T, ctx context.Context, arc string, args ...string) *ttyRun {
	t.Helper()
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err = pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	// The shell keeps the terminal open after arc ends, so the test can read
	// the terminal state. It prints the exit status of arc.
	script := `"$@"; result=$?; printf "\narc-exit-%s\n" "$result"; read stop`
	command := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh", arc}, args...)...)
	r := &ttyRun{command: command, terminal: terminal, slave: slave, out: &output{}}
	if r.original, err = term.GetState(int(slave.Fd())); err != nil {
		t.Fatal(err)
	}
	r.command.Stdin, r.command.Stdout, r.command.Stderr = slave, slave, slave
	r.command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err = r.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { io.Copy(r.out, terminal) }()
	t.Cleanup(func() {
		_ = r.command.Process.Kill()
		slave.Close()
		terminal.Close()
	})
	return r
}

// detach types the escape, and checks that arc ends with status 0 and
// restores the terminal.
func (r *ttyRun) detach(t *testing.T) {
	t.Helper()
	io.WriteString(r.terminal, "\r~.")
	if !waitFor(r.out, "arc-exit-", 5*time.Second) {
		t.Fatalf("arc did not end after the detach escape: %s", r.out.String())
	}
	if !waitFor(r.out, "arc-exit-0\r\n", time.Second) || !strings.Contains(r.out.String(), "arc: detached") {
		t.Fatalf("detach: %s", r.out.String())
	}
	restored, err := term.GetState(int(r.slave.Fd()))
	if err != nil || *restored != *r.original {
		t.Fatalf("the terminal was not restored: %v", err)
	}
	io.WriteString(r.terminal, "\n")
	if err := r.command.Wait(); err != nil {
		t.Fatal(err)
	}
}

// A person starts a kept process under a terminal, detaches with the escape,
// and attaches again. The process runs while no terminal is attached.
func TestTerminalDetachAndAttachThroughCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	consumer, key := providerHome(t, relay)
	execHome, execKey := providerHome(t, relay)
	config := filepath.Join(t.TempDir(), "exec.json")
	data, _ := json.Marshal(map[string]any{"grants": []string{key}, "cwd": t.TempDir()})
	os.WriteFile(config, data, 0600)
	t.Setenv("EXEC_CONFIG", config)
	serveSessionProvider(t, ctx, execHome, providerBinary(t, "../../apps/exec/cmd/arc-exec"), "../../apps/exec/manifest.json")
	ok(t, consumer, "", "install", execKey, "--yes")
	arc := providerBinary(t, ".")
	address := "exec+arc://" + execKey + "/"

	first := startTTY(t, ctx, arc, "--home", consumer, "session", "--tty", "--timeout", "30s", address,
		`{"script":"stty -echo; printf tty-ready; while read w; do printf 'got-%s\\n' \"$w\"; done","keep":"alfred"}`)
	if !waitFor(first.out, "tty-ready", 5*time.Second) {
		t.Fatalf("CLI PTY: %s", first.out.String())
	}
	// A tilde inside a line goes to the process.
	io.WriteString(first.terminal, "one~.\r")
	if !waitFor(first.out, "got-one~.", 5*time.Second) {
		t.Fatalf("input: %s", first.out.String())
	}
	first.detach(t)
	if list := ok(t, consumer, "", "exec", "list"); !strings.Contains(list, "alfred") || !strings.Contains(list, "running") {
		t.Fatalf("the process ended with the detach: %s", list)
	}

	second := startTTY(t, ctx, arc, "--home", consumer, "session", "--tty", "--timeout", "30s", address, `{"attach":"alfred"}`)
	if !waitFor(second.out, "got-one~.", 5*time.Second) {
		t.Fatalf("the attach did not show the buffer: %s", second.out.String())
	}
	io.WriteString(second.terminal, "two\r")
	if !waitFor(second.out, "got-two", 5*time.Second) {
		t.Fatalf("input after attach: %s", second.out.String())
	}
	second.detach(t)
	ok(t, consumer, "", "exec", "kill", "alfred")
}
