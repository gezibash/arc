package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gezibash/arc/core/keys"
	"golang.org/x/sys/unix"
)

// echoes says whether the terminal shows what is typed on it.
func echoes(t *testing.T, tty *os.File) bool {
	t.Helper()
	termios, err := unix.IoctlGetTermios(int(tty.Fd()), getTermios)
	if err != nil {
		t.Fatal(err)
	}
	return termios.Lflag&unix.ECHO != 0
}

// sealedHome is a home whose key a passphrase seals.
func sealedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	t.Setenv("ARC_PASSPHRASE", "open sesame")
	ok(t, home, "", "keys", "encrypt")
	t.Setenv("ARC_PASSPHRASE", "")
	return home
}

// atPrompt starts arc in home on a new terminal, through sh with the
// script before it, and waits for the prompt of the passphrase. keys
// account asks the signer at once, so arc-signer asks for the passphrase.
func atPrompt(t *testing.T, home, script string) (command *exec.Cmd, terminal *os.File, shown *output, ended chan error) {
	t.Helper()
	arc := providerBinary(t, ".")
	command = exec.CommandContext(t.Context(), "sh", "-c", script+` exec "$0" "$@"`, arc, "--home", home, "keys", "account", keys.Generate().Public.Hex())
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.Close(); _ = slave.Close() })
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	shown = &output{}
	go func() { _, _ = io.Copy(shown, terminal) }()
	ended = make(chan error, 1)
	go func() { ended <- command.Wait() }()
	if !waitFor(shown, "passphrase of the key: ", 10*time.Second) {
		t.Fatalf("no prompt: %q", shown.String())
	}
	time.Sleep(100 * time.Millisecond)
	return command, terminal, shown, ended
}

// endedBy waits a few seconds for arc to end by the signal.
func endedBy(t *testing.T, command *exec.Cmd, ended chan error, want syscall.Signal) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatalf("arc did not end within 5 s of %v at the prompt", want)
	}
	status, _ := command.ProcessState.Sys().(syscall.WaitStatus)
	if want == syscall.SIGQUIT {
		// Go ends on SIGQUIT with a dump of its goroutines and exit 2.
		if !status.Signaled() && status.ExitStatus() != 2 {
			t.Fatalf("after %v arc ended with %v", want, command.ProcessState)
		}
		return
	}
	if !status.Signaled() || status.Signal() != want {
		t.Fatalf("after %v arc ended with %v", want, command.ProcessState)
	}
}

// An interrupt or a quit at the prompt ends arc by that signal, and leaves
// the terminal echoing.
func TestASignalAtThePromptRestoresEcho(t *testing.T) {
	home := sealedHome(t)
	for _, c := range []struct {
		key    byte
		signal syscall.Signal
	}{{3, syscall.SIGINT}, {0x1c, syscall.SIGQUIT}} {
		command, terminal, _, ended := atPrompt(t, home, "")
		_, _ = terminal.Write([]byte{c.key})
		endedBy(t, command, ended, c.signal)
		// The other side keeps the mode of the terminal: macOS revokes this
		// side when arc, which leads its session, ends.
		if !echoes(t, terminal) {
			t.Fatalf("after %v at the prompt the terminal does not echo", c.signal)
		}
	}
}

// arc started with interrupts ignored, as under nohup or trap ” INT, keeps
// ignoring them at the prompt, and keeps echo off.
func TestAnIgnoredInterruptAtThePromptKeepsEchoOff(t *testing.T) {
	home := sealedHome(t)
	command, terminal, shown, ended := atPrompt(t, home, "trap '' INT;")
	_, _ = terminal.Write([]byte{3}) // Ctrl-C
	// Within the second in which a watch of the signal would have echo on.
	time.Sleep(300 * time.Millisecond)
	_, _ = io.WriteString(terminal, "open sesame\n")
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("arc did not go on after the passphrase")
	}
	if status, _ := command.ProcessState.Sys().(syscall.WaitStatus); status.Signaled() {
		t.Fatalf("an ignored interrupt ended arc: %v", command.ProcessState)
	}
	if strings.Contains(shown.String(), "open sesame") {
		t.Fatalf("the terminal showed the passphrase: %q", shown.String())
	}
}
