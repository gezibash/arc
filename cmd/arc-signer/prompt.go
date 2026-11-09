package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gezibash/arc/sdk/terminal"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// askPassphrase reads ARC_PASSPHRASE, or asks on the terminal without echo.
func askPassphrase(prompt string) (string, error) {
	return askSecret("ARC_PASSPHRASE", prompt)
}

// openTTY opens the terminal of arc; tests give another.
var openTTY = func() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

// afterPrompt runs when the prompt shows; tests type there.
var afterPrompt = func() {}

// readPassword reads a line without echo; tests make it fail.
var readPassword = term.ReadPassword

// askSecret reads the variable env, or asks on the terminal without echo.
// Echo goes off before the prompt shows, so an answer typed or pasted as
// the prompt appears is not shown, and it comes back on each way out, also
// on a signal that ends arc.
func askSecret(env, prompt string) (string, error) {
	if value := os.Getenv(env); value != "" {
		return value, nil
	}
	tty, err := openTTY()
	if err != nil {
		return "", fmt.Errorf("arc needs a passphrase: set %s, or run arc on a terminal", env)
	}
	defer func() { _ = tty.Close() }()
	fd := int(tty.Fd())
	echo := &quietTerminal{fd: fd}
	// The watch of signals starts before echo goes off, and stops after
	// it comes back, so no signal falls between the two.
	defer watchSignals(echo)()
	if err := echo.off(); err != nil {
		return "", err
	}
	defer echo.restore()
	fmt.Fprint(tty, prompt)
	afterPrompt()
	value, err := readPassword(fd)
	fmt.Fprintln(tty)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

// quietTerminal turns the echo of a terminal off, and brings back the mode
// that it had.
type quietTerminal struct {
	fd    int
	mu    sync.Mutex
	saved *unix.Termios // the mode to bring back; nil while echo is on
}

// off stops the terminal from showing what is typed.
func (q *quietTerminal) off() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.saved != nil {
		return nil
	}
	old, err := unix.IoctlGetTermios(q.fd, getTermios)
	if err != nil {
		return err
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	quiet.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(q.fd, setTermios, &quiet); err != nil {
		return err
	}
	q.saved = old
	return nil
}

// restore brings back the mode that the terminal had before off.
func (q *quietTerminal) restore() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.saved == nil {
		return
	}
	if err := unix.IoctlSetTermios(q.fd, setTermios, q.saved); err != nil {
		fmt.Fprintln(os.Stderr, "arc: the terminal may still not echo; run stty echo:", terminal.EscapeLine(err.Error()))
	}
	q.saved = nil
}

// promptSignals end arc; at the prompt, echo comes back before they do.
var promptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// watchSignals brings the echo of q back when a signal that ends arc
// comes, and then lets the signal end arc. It leaves a signal that arc
// ignores alone: a watch would stop the ignore. The returned function stops
// the watch.
func watchSignals(q *quietTerminal) (stop func()) {
	var watched []os.Signal
	for _, s := range promptSignals {
		if !signal.Ignored(s) {
			watched = append(watched, s)
		}
	}
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	if len(watched) == 0 {
		return func() {}
	}
	signal.Notify(signals, watched...)
	go func() {
		select {
		case got := <-signals:
			q.restore()
			signal.Reset(got)
			if sig, ok := got.(syscall.Signal); ok {
				_ = syscall.Kill(os.Getpid(), sig)
			}
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}
