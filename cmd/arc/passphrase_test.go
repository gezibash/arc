package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
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

// onPTY makes askSecret ask on a new terminal, and returns its two sides
// and what the terminal shows.
func onPTY(t *testing.T) (terminal, slave *os.File, shown *output) {
	t.Helper()
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.Close(); _ = slave.Close() })
	shown = &output{}
	go func() { _, _ = io.Copy(shown, terminal) }()
	opened, prompted, read := openTTY, afterPrompt, readPassword
	t.Cleanup(func() { openTTY, afterPrompt, readPassword = opened, prompted, read })
	openTTY = func() (*os.File, error) {
		fd, err := unix.Dup(int(slave.Fd()))
		return os.NewFile(uintptr(fd), "tty"), err
	}
	t.Setenv("ARC_PASSPHRASE", "")
	return terminal, slave, shown
}

// An answer typed as soon as the prompt shows is not shown: echo is off
// before the prompt, not after it. The terminal echoes again afterwards.
func TestAnAnswerTypedAtThePromptIsNotShown(t *testing.T) {
	terminal, slave, shown := onPTY(t)
	afterPrompt = func() {
		_, _ = io.WriteString(terminal, "open sesame\n")
		time.Sleep(200 * time.Millisecond) // the terminal would echo now
	}
	got, err := askPassphrase("passphrase of the key: ")
	if err != nil || got != "open sesame" {
		t.Fatalf("askPassphrase gave %q, %v", got, err)
	}
	if !waitFor(shown, "passphrase of the key: ", 5*time.Second) {
		t.Fatalf("no prompt: %q", shown.String())
	}
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(shown.String(), "open sesame") {
		t.Fatalf("the terminal showed the passphrase: %q", shown.String())
	}
	if !echoes(t, slave) {
		t.Fatal("the terminal does not echo after the prompt")
	}
}

// A read that fails, as after a hangup, still gives echo back.
func TestAFailedReadRestoresEcho(t *testing.T) {
	_, slave, _ := onPTY(t)
	readPassword = func(int) ([]byte, error) { return nil, syscall.EIO }
	if _, err := askPassphrase("passphrase of the key: "); !errors.Is(err, syscall.EIO) {
		t.Fatalf("askPassphrase gave %v, want EIO", err)
	}
	if !echoes(t, slave) {
		t.Fatal("the terminal does not echo after a read that failed")
	}
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
// script before it, and waits for the prompt of the passphrase.
func atPrompt(t *testing.T, home, script string) (command *exec.Cmd, terminal *os.File, shown *output, ended chan error) {
	t.Helper()
	arc := providerBinary(t, ".")
	command = exec.CommandContext(t.Context(), "sh", "-c", script+` exec "$0" "$@"`, arc, "--home", home, "message", "inbox")
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

// After the prompt, arc lets go of the signals: an interrupt ends it as
// before. The helper below asks on a terminal, then interrupts itself.
func TestThePromptLetsGoOfItsSignals(t *testing.T) {
	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPromptHelper$")
	helper.Env = append(os.Environ(), "ARC_PROMPT_HELPER=1")
	out, err := helper.CombinedOutput()
	status, _ := helper.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("an interrupt after the prompt did not end the helper: %v\n%s", err, out)
	}
}

func TestPromptHelper(t *testing.T) {
	if os.Getenv("ARC_PROMPT_HELPER") != "1" {
		t.Skip("runs only inside TestThePromptLetsGoOfItsSignals")
	}
	terminal, _, _ := onPTY(t)
	afterPrompt = func() { _, _ = io.WriteString(terminal, "open sesame\n") }
	if _, err := askPassphrase("passphrase of the key: "); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	time.Sleep(3 * time.Second)
	t.Log("the helper outlived an interrupt")
}
