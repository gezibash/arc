package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/iface"
	execadapter "github.com/gezibash/arc/sdk/execadapter"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func terminalRequest(input io.Reader, body string) (string, error) {
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", fmt.Errorf("--tty requires a terminal")
	}
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err != nil || request == nil {
		return "", fmt.Errorf("terminal request must be a JSON object")
	}
	cols, rows, err := term.GetSize(int(file.Fd()))
	if err != nil {
		return "", err
	}
	request["pty"] = true
	request["rows"] = rows
	request["cols"] = cols
	data, err := json.Marshal(request)
	return string(data), err
}
func execSession(command *cobra.Command, stream *session.Stream, tty bool) error {
	input := command.InOrStdin()
	if tty {
		held, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer held.Close()
		input = held
	}
	var mu sync.Mutex
	encoder := json.NewEncoder(stream)
	send := func(r execadapter.Record) error { mu.Lock(); defer mu.Unlock(); return encoder.Encode(r) }
	if tty {
		file := command.InOrStdin().(*os.File)
		old, err := term.MakeRaw(int(file.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(file.Fd()), old)
		resize := make(chan os.Signal, 1)
		signal.Notify(resize, syscall.SIGWINCH)
		defer signal.Stop(resize)
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-resize:
					cols, rows, err := term.GetSize(int(file.Fd()))
					if err == nil {
						if err = send(execadapter.Record{Type: "resize", Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
							return
						}
					}
				case <-stream.Context().Done():
					return
				case <-done:
					return
				}
			}
		}()
	}
	inputFailure := make(chan error, 1)
	if stream.Mode() == session.Duplex {
		// The terminal reader polls its own context and is joined before closing.
		if !tty {
			defer closeOnEnd(stream, input)()
		}
		readInput := input
		inputCtx, stopInput := context.WithCancel(stream.Context())
		if tty {
			readInput = terminalReader{inputCtx, int(input.(*os.File).Fd())}
		}
		inputDone := make(chan struct{})
		defer func() {
			stopInput()
			if tty {
				<-inputDone
			}
		}()
		go func() {
			defer close(inputDone)
			buf := make([]byte, 4096)
			for {
				n, err := readInput.Read(buf)
				if n > 0 {
					if e := send(execadapter.Record{Type: "stdin", Data: buf[:n]}); e != nil {
						return
					}
				}
				if errors.Is(err, io.EOF) {
					_ = stream.CloseWrite()
					return
				}
				if err != nil {
					inputFailure <- err
					_ = stream.Close()
					return
				}
			}
		}()
	}
	reader := execadapter.NewReader(stream)
	exit := -1
	for {
		record, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			select {
			case inputErr := <-inputFailure:
				return fmt.Errorf("exec input: %w", inputErr)
			default:
			}
			return err
		}
		if exit >= 0 {
			return fmt.Errorf("exec output after exit status")
		}
		switch record.Type {
		case "stdout":
			_, err = command.OutOrStdout().Write(record.Data)
		case "stderr":
			_, err = command.ErrOrStderr().Write(record.Data)
		case "exit":
			if record.Exit < 0 || record.Exit > 255 {
				return fmt.Errorf("invalid Exec exit status")
			}
			exit = record.Exit
		default:
			return fmt.Errorf("unknown Exec record %q", record.Type)
		}
		if err != nil {
			return err
		}
	}
	if err := stream.Wait(); err != nil {
		return err
	}
	if exit < 0 {
		return fmt.Errorf("exec session ended without exit status")
	}
	if exit != 0 {
		return iface.ExitError{Code: exit}
	}
	return nil
}

// Some terminal descriptors cannot join the Go poller on macOS. Poll the
// nonblocking descriptor explicitly so cancellation can always stop input.
type terminalReader struct {
	ctx context.Context
	fd  int
}

func (r terminalReader) Read(p []byte) (int, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		n, err = unix.Read(r.fd, p)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err == nil && n == 0 {
			return 0, io.EOF
		}
		return n, err
	}
}
