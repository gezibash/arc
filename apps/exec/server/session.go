package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	execadapter "github.com/gezibash/arc/sdk/execadapter"
	"github.com/gezibash/arc/sdk/provider"
	"golang.org/x/sys/unix"
)

func (s *server) HandleSession(parent context.Context, req provider.Request, stream *provider.Stream) error {
	if req.Method() != "EXEC" {
		return provider.ErrInvalidRequest
	}
	if !s.config.Grants[req.From] {
		return provider.Error("access_denied")
	}
	request, err := decodeBody(s.config, req.Message)
	if err != nil {
		return err
	}
	if request.Keep != "" || request.Attach != "" {
		return s.keptSession(parent, req.From, request, stream)
	}
	if request.Action != "" && request.Action != "run" || request.Job != "" || request.Name != "" {
		return provider.ErrInvalidRequest
	}
	if request.PTY && stream.Mode() != provider.Duplex {
		return provider.ErrUnsupported
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
	commandDone := new(atomic.Bool)
	if request.PTY {
		terminal, err = startTerminal(process, request.Rows, request.Cols)
		if err != nil {
			return err
		}
		input = terminal
		outputDone = make(chan error, 1)
		go func() {
			_, err := io.Copy(processChannel{output, "stdout"}, ptyOutput{terminal, commandDone})
			// EIO: no process holds the terminal open. A deadline: no output
			// came for drainIdle after the command exited.
			if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrDeadlineExceeded) {
				err = nil
			}
			if err != nil {
				cancel(err)
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
		_ = rd.Close()
		if err != nil {
			_ = wr.Close()
			return err
		}
		input = wr
	}
	s.lease.hold()
	defer s.lease.release()
	defer func() { _ = input.Close() }()
	interruptDone := make(chan struct{})
	interrupt := context.AfterFunc(ctx, func() {
		defer close(interruptDone)
		killGroup(process)
		_ = input.Close()
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
		if stream.Mode() != provider.Duplex {
			if err := input.Close(); err != nil && ctx.Err() == nil && !inputGone(err) {
				cancel(err)
			}
			return
		}
		reader := execadapter.NewReader(stream)
		for {
			record, err := reader.Next()
			if errors.Is(err, io.EOF) {
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
		commandDone.Store(true)
		// This deadline also ends a read that started before the exit. If
		// it fails, the next read reports the failure.
		_ = terminal.SetReadDeadline(time.Now().Add(drainIdle))
		err = <-outputDone
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

// keptSession starts a kept process or attaches to one. Both need a duplex
// session.
func (s *server) keptSession(ctx context.Context, from string, request body, stream *provider.Stream) error {
	if request.Action != "" && request.Action != "run" || request.Job != "" || request.Name != "" {
		return provider.ErrInvalidRequest
	}
	if stream.Mode() != provider.Duplex {
		return provider.ErrUnsupported
	}
	if request.Attach != "" {
		if request.Keep != "" || request.Argv != nil || request.Script != "" || request.CWD != "" ||
			request.Stdin != "" || request.TimeoutMS != nil {
			return provider.ErrInvalidRequest
		}
		k := s.find(request.Attach)
		if k == nil {
			return provider.Error("not_found")
		}
		if request.PTY != (k.terminal != nil) {
			return provider.Error("pty_mismatch")
		}
		return s.attach(ctx, k, stream, request.Rows, request.Cols)
	}
	if !keepNamePattern.MatchString(request.Keep) {
		return provider.Error("keep must be 1 to 64 letters, digits, '.', '_' or '-'")
	}
	if request.TimeoutMS != nil {
		return provider.Error("a kept process has no timeout_ms")
	}
	cmd, err := parseCommand(s.config, request, s.config.Limits.TimeoutMS)
	if err != nil {
		return err
	}
	k, err := s.keep(from, request.Keep, cmd, request)
	if err != nil {
		return err
	}
	return s.attach(ctx, k, stream, 0, 0)
}

// startTerminal starts the process on a new terminal, 24 by 80 by default.
// It returns the nonblocking master of the terminal.
func startTerminal(process *exec.Cmd, rows, cols uint16) (*os.File, error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	terminal, err := pty.StartWithSize(process, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
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
			_ = syscall.Close(fd)
		}
		killGroup(process)
		_ = process.Wait()
		_ = terminal.Close()
		return nil, dupErr
	}
	_ = terminal.Close()
	return os.NewFile(uintptr(fd), "exec-pty"), nil
}

// drainIdle bounds the wait for terminal output after the command exits,
// because a background process can hold the terminal open.
const drainIdle = time.Second

// ptyOutput reads the terminal. After the command exits, each read waits at
// most drainIdle for new output. The time that a slow peer takes to
// acknowledge earlier output does not count.
type ptyOutput struct {
	terminal *os.File
	exited   *atomic.Bool
}

func (r ptyOutput) Read(p []byte) (int, error) {
	if r.exited.Load() {
		if err := r.terminal.SetReadDeadline(time.Now().Add(drainIdle)); err != nil {
			return 0, err
		}
	}
	return r.terminal.Read(p)
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
