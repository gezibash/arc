package main

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
)

// An app command reads nothing from a terminal, so arc journal write with
// no text ends at once instead of waiting for input (proposal journal-v2,
// section 4.3: no command waits for a TTY).
func TestAnAppCommandDoesNotWaitForATerminal(t *testing.T) {
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Skip("no terminal on this machine:", err)
	}
	defer terminal.Close()
	defer slave.Close()

	read := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(inputOf(slave))
		read <- data
	}()
	select {
	case data := <-read:
		if len(data) != 0 {
			t.Errorf("read %q from a terminal", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the command waits for the terminal")
	}

	// A pipe still gives its text.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("piped text\n")
	w.Close()
	data, err := io.ReadAll(inputOf(r))
	if err != nil || string(data) != "piped text\n" {
		t.Errorf("a pipe gave %q, %v", data, err)
	}
}
