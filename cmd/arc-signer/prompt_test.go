package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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

// output keeps what a terminal shows, for a test that reads it while the
// prompt runs.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func waitFor(out *output, text string, limit time.Duration) bool {
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(out.String(), text) {
			return true
		}
	}
	return false
}
