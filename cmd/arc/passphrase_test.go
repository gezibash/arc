package main

import (
	"context"
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

// An answer typed as soon as the prompt shows is not shown: echo is off
// before the prompt, not after it. The terminal echoes again afterwards.
func TestAnAnswerTypedAtThePromptIsNotShown(t *testing.T) {
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer slave.Close()
	shown := &output{}
	go func() { _, _ = io.Copy(shown, terminal) }()

	opened, prompted := openTTY, afterPrompt
	t.Cleanup(func() { openTTY, afterPrompt = opened, prompted })
	openTTY = func() (*os.File, error) {
		fd, err := unix.Dup(int(slave.Fd()))
		return os.NewFile(uintptr(fd), "tty"), err
	}
	afterPrompt = func() {
		_, _ = io.WriteString(terminal, "open sesame\n")
		time.Sleep(200 * time.Millisecond) // the terminal would echo now
	}
	t.Setenv("ARC_PASSPHRASE", "")
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

// An interrupt at the prompt ends arc, and leaves the terminal echoing.
func TestAnInterruptAtThePromptRestoresEcho(t *testing.T) {
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	t.Setenv("ARC_PASSPHRASE", "open sesame")
	ok(t, home, "", "keys", "encrypt")
	t.Setenv("ARC_PASSPHRASE", "")

	arc := providerBinary(t, ".")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, arc, "--home", home, "message", "inbox")
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer slave.Close()
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	shown := &output{}
	go func() { _, _ = io.Copy(shown, terminal) }()
	if !waitFor(shown, "passphrase of the key: ", 10*time.Second) {
		t.Fatalf("no prompt: %q", shown.String())
	}
	time.Sleep(100 * time.Millisecond)
	_, _ = terminal.Write([]byte{3}) // Ctrl-C
	err = command.Wait()
	if err == nil {
		t.Fatal("arc ended well after an interrupt at the prompt")
	}
	// The other side keeps the mode of the terminal: macOS revokes this
	// side when arc, which leads its session, ends.
	if !echoes(t, terminal) {
		t.Fatalf("after an interrupt at the prompt the terminal does not echo (arc: %v)", err)
	}
}
