package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	execadapter "github.com/gezibash/arc/adapters/exec"
	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"golang.org/x/sys/unix"
)

func (s *server) HandleSession(parent context.Context, req provider.Request, stream *session.Stream) error {
	if req.Method() != "EXEC" {
		return provider.ErrInvalidRequest
	}
	if !s.config.Grants[req.From] {
		return provider.Error("access_denied")
	}
	if len(req.Message) > s.config.Limits.BodyBytes {
		return provider.Error("request_too_large")
	}
	var request body
	decoder := json.NewDecoder(strings.NewReader(req.Message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return provider.ErrInvalidRequest
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return provider.ErrInvalidRequest
	}
	if request.Action != "" && request.Action != "run" || request.Job != "" {
		return provider.ErrInvalidRequest
	}
	if request.PTY && stream.Mode() != session.Duplex {
		return session.ErrUnsupported
	}
	cmd, err := parseCommand(s.config, request, s.config.Limits.TimeoutMS)
	if err != nil {
		return err
	}
	ctx, stop := context.WithTimeout(parent, time.Duration(cmd.TimeoutMS)*time.Millisecond)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	process := start(cmd)
	process.WaitDelay = time.Second
	output := &processOutput{encoder: json.NewEncoder(stream), remaining: s.config.Limits.OutputBytes, cancel: cancel}
	var input io.WriteCloser
	var terminal *os.File
	var outputDone chan error
	if request.PTY {
		rows, cols := request.Rows, request.Cols
		if rows == 0 {
			rows = 24
		}
		if cols == 0 {
			cols = 80
		}
		terminal, err = pty.StartWithSize(process, &pty.Winsize{Rows: rows, Cols: cols})
		if err != nil {
			return err
		}
		// Register a nonblocking duplicate with Go's poller. The original
		// pty descriptor was wrapped before O_NONBLOCK was set.
		fd, dupErr := syscall.Dup(int(terminal.Fd()))
		if dupErr == nil {
			syscall.CloseOnExec(fd)
			dupErr = syscall.SetNonblock(fd, true)
		}
		if dupErr != nil {
			if fd >= 0 {
				syscall.Close(fd)
			}
			killGroup(process)
			process.Wait()
			terminal.Close()
			return dupErr
		}
		terminal.Close()
		terminal = os.NewFile(uintptr(fd), "exec-pty")
		input = terminal
		outputDone = make(chan error, 1)
		go func() {
			_, err := io.Copy(processChannel{output, "stdout"}, terminal)
			if err != nil && !errors.Is(err, syscall.EIO) {
				cancel(err)
			}
			if errors.Is(err, syscall.EIO) {
				err = nil
			}
			outputDone <- err
		}()
	} else {
		rd, wr, e := os.Pipe()
		if e != nil {
			return e
		}
		process.Stdin = rd
		process.Stdout = processChannel{output, "stdout"}
		process.Stderr = processChannel{output, "stderr"}
		err = process.Start()
		rd.Close()
		if err != nil {
			wr.Close()
			return err
		}
		input = wr
	}
	s.lease.hold()
	defer s.lease.release()
	defer input.Close()
	interruptDone := make(chan struct{})
	interrupt := context.AfterFunc(ctx, func() {
		defer close(interruptDone)
		killGroup(process)
		input.Close()
		stream.Abort(context.Cause(ctx))
	})
	defer func() {
		if !interrupt() {
			<-interruptDone
		}
	}()
	defer killGroup(process)
	// Core closes the stream after the handler returns, releasing a pending read.
	go func() {
		if len(cmd.Stdin) > 0 {
			if _, err := input.Write(cmd.Stdin); err != nil {
				if !inputGone(err) {
					cancel(err)
				}
				return
			}
		}
		if stream.Mode() != session.Duplex {
			input.Close()
			return
		}
		reader := execadapter.NewReader(stream)
		for {
			record, err := reader.Next()
			if err == io.EOF {
				if terminal != nil {
					_, err = input.Write([]byte{4})
				} else {
					err = input.Close()
				}
				if err != nil && ctx.Err() == nil && !inputGone(err) {
					cancel(err)
				}
				return
			}
			if err != nil {
				cancel(err)
				return
			}
			switch record.Type {
			case "stdin":
				_, err = input.Write(record.Data)
			case "resize":
				if terminal == nil || record.Rows == 0 || record.Cols == 0 {
					err = provider.ErrInvalidRequest
				} else {
					err = setsize(terminal, record.Rows, record.Cols)
				}
			default:
				err = provider.ErrInvalidRequest
			}
			if err != nil {
				if !inputGone(err) {
					cancel(err)
				}
				return
			}
		}
	}()
	waitErr := process.Wait()
	killGroup(process)
	if terminal != nil {
		timer := time.AfterFunc(time.Second, func() { terminal.Close() })
		err = <-outputDone
		timer.Stop()
		if err != nil && ctx.Err() == nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	var exited interface{ ExitCode() int }
	if waitErr != nil && !errors.As(waitErr, &exited) {
		return waitErr
	}
	exit := process.ProcessState.ExitCode()
	if exit < 0 {
		exit = 1
	}
	return output.encoder.Encode(execadapter.Record{Type: "exit", Exit: exit})
}

type processOutput struct {
	mu        sync.Mutex
	encoder   *json.Encoder
	remaining int
	cancel    context.CancelCauseFunc
}
type processChannel struct {
	output *processOutput
	name   string
}

func (w processChannel) Write(p []byte) (int, error) {
	w.output.mu.Lock()
	defer w.output.mu.Unlock()
	total := 0
	for len(p) > 0 {
		n := min(len(p), 4096)
		if n > w.output.remaining {
			err := provider.Error("output_too_large")
			w.output.cancel(err)
			return total, err
		}
		if err := w.output.encoder.Encode(execadapter.Record{Type: w.name, Data: p[:n]}); err != nil {
			w.output.cancel(err)
			return total, err
		}
		w.output.remaining -= n
		total += n
		p = p[n:]
	}
	return total, nil
}

// inputGone reports that the command no longer reads its input, usually
// because it exited. The exit status, not the write error, is the outcome.
func inputGone(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EIO)
}

// setsize resizes the terminal through the raw connection. pty.Setsize calls
// File.Fd, which puts the nonblocking master back into blocking mode.
func setsize(terminal *os.File, rows, cols uint16) error {
	conn, err := terminal.SyscallConn()
	if err != nil {
		return err
	}
	controlErr := conn.Control(func(fd uintptr) {
		err = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
	if controlErr != nil {
		return controlErr
	}
	return err
}
