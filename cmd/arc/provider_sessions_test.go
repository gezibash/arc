package main

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gezibash/arc/internal/testrelay"
	"golang.org/x/term"
)

// programWith wraps a test program in a script that gives it the
// environment env, as NAME=value. A test that configures its provider so
// needs no t.Setenv, and can run in parallel with the others.
func programWith(t *testing.T, binary string, env ...string) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	script := "#!/bin/sh\n"
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		script += "export " + name + "=" + quote(value) + "\n"
	}
	script += "exec " + quote(binary) + ` "$@"` + "\n"
	path := filepath.Join(t.TempDir(), "program.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// builtProgram is one test program, built once for each run of the tests.
type builtProgram struct {
	once sync.Once
	path string
	err  error
}

// built holds the programs that providerBinary built, by package.
var built sync.Map

// providerBinary builds the program of pkg once for each run of the tests,
// and gives its path. A test must not change the file: each test that
// builds pkg gets the same one.
func providerBinary(t *testing.T, pkg string) string {
	t.Helper()
	abs, err := filepath.Abs(pkg)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := built.LoadOrStore(abs, &builtProgram{})
	b := v.(*builtProgram)
	b.once.Do(func() {
		dir, err := os.MkdirTemp(programs, "program-")
		if err != nil {
			b.err = err
			return
		}
		b.path = filepath.Join(dir, filepath.Base(abs))
		cmd := exec.Command("go", "build", "-o", b.path, abs)
		cmd.Stderr = os.Stderr
		b.err = cmd.Run()
	})
	if b.err != nil {
		t.Fatal(b.err)
	}
	return b.path
}

// providerHome makes a citizen with one relay, and gives its home and its
// public key.
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
	waitServes(t, out, done, 10*time.Second)
}

// arc session --tty runs under a real terminal: it forwards the size and
// the raw input to an exec session, and puts the terminal back as it found
// it when the program ends. The apps that serve sessions are tested in
// their own packages; TestCLISessionKeepsStateAndProducesOutputBeforeInputEOF
// runs a session through a relay and the normal CLI.
func TestATerminalSessionRestoresTheTerminalThroughCLI(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	consumer, key := providerHome(t, relay)
	execHome, execKey := providerHome(t, relay)
	config := filepath.Join(t.TempDir(), "exec.json")
	data, _ := json.Marshal(map[string]any{"grants": []string{key}, "cwd": t.TempDir()})
	os.WriteFile(config, data, 0600)
	serveSessionProvider(t, ctx, execHome, programWith(t, providerBinary(t, "../../apps/exec/cmd/arc-exec"), "EXEC_CONFIG="+config), "../../apps/exec/manifest.json")
	ok(t, consumer, "", "install", execKey, "--yes")
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
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	consumer, key := providerHome(t, relay)
	execHome, execKey := providerHome(t, relay)
	config := filepath.Join(t.TempDir(), "exec.json")
	data, _ := json.Marshal(map[string]any{"grants": []string{key}, "cwd": t.TempDir()})
	os.WriteFile(config, data, 0600)
	serveSessionProvider(t, ctx, execHome, programWith(t, providerBinary(t, "../../apps/exec/cmd/arc-exec"), "EXEC_CONFIG="+config), "../../apps/exec/manifest.json")
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
