package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	execadapter "github.com/gezibash/arc/sdk/execadapter"
	"github.com/gezibash/arc/sdk/provider"
	"golang.org/x/sys/unix"
)

// A kept process outlives the session that starts it. A later session
// attaches to it by its name. The process ends when it exits, when a caller
// kills it, or when the provider stops. It has no time limit.
//
// Each provider has one set of names. Each granted caller can attach to,
// list and kill each kept process.

var keepNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// errDetached ends a session when another session attaches to its process.
var errDetached = provider.Error("detached")

// keeper holds the kept processes of one provider by name.
type keeper struct {
	mu      sync.Mutex
	byName  map[string]*keptProcess
	stopped bool
}

type keptProcess struct {
	name     string
	owner    string
	started  time.Time
	process  *exec.Cmd
	terminal *os.File
	// ended closes after the process exited and its output is in the buffer.
	ended chan struct{}

	inputMu sync.Mutex
	input   io.WriteCloser

	mu      sync.Mutex
	changed chan struct{}
	output  outputBuffer
	exited  bool
	exit    int
	viewer  *viewer
}

// viewer is the one session that is attached to a kept process.
type viewer struct {
	detach context.CancelCauseFunc
}

// keep starts a kept process with the name. The name must be free.
func (s *server) keep(owner, name string, cmd *command, request body) (*keptProcess, error) {
	s.kept.mu.Lock()
	defer s.kept.mu.Unlock()
	if s.kept.stopped {
		return nil, provider.Error("provider_stopping")
	}
	if _, used := s.kept.byName[name]; used {
		return nil, provider.Error("name_in_use")
	}
	k := &keptProcess{
		name:    name,
		owner:   owner,
		started: time.Now(),
		ended:   make(chan struct{}),
		changed: make(chan struct{}),
		output:  outputBuffer{limit: s.config.Limits.OutputBytes},
	}
	process := start(cmd)
	if request.PTY {
		terminal, err := startTerminal(process, request.Rows, request.Cols)
		if err != nil {
			return nil, err
		}
		k.terminal = terminal
		k.input = terminal
	} else {
		rd, wr, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		process.Stdin = rd
		process.Stdout = keptOutput{k, "stdout"}
		process.Stderr = keptOutput{k, "stderr"}
		process.WaitDelay = time.Second
		err = process.Start()
		_ = rd.Close()
		if err != nil {
			_ = wr.Close()
			return nil, err
		}
		k.input = wr
	}
	k.process = process
	s.lease.hold()
	go s.watch(k)
	if len(cmd.Stdin) > 0 {
		go func() { _ = k.write(cmd.Stdin) }()
	}
	if s.kept.byName == nil {
		s.kept.byName = map[string]*keptProcess{}
	}
	s.kept.byName[name] = k
	return k, nil
}

// watch reads the terminal of the process, waits for its exit, and records
// the exit status. It holds the lease while the process runs.
func (s *server) watch(k *keptProcess) {
	defer s.lease.release()
	commandDone := new(atomic.Bool)
	read := make(chan struct{})
	if k.terminal != nil {
		go func() {
			defer close(read)
			_, _ = io.Copy(keptOutput{k, "stdout"}, ptyOutput{k.terminal, commandDone})
		}()
	}
	_ = k.process.Wait()
	killGroup(k.process)
	if k.terminal != nil {
		commandDone.Store(true)
		_ = k.terminal.SetReadDeadline(time.Now().Add(drainIdle))
		<-read
	}
	exit := k.process.ProcessState.ExitCode()
	if exit < 0 {
		exit = 1
	}
	k.mu.Lock()
	k.exited = true
	k.exit = exit
	k.notify()
	k.mu.Unlock()
	// The input closes after the exit is recorded, so a late write or
	// resize of a session is not an error of the session.
	k.inputMu.Lock()
	_ = k.input.Close()
	k.inputMu.Unlock()
	close(k.ended)
}

// attach connects the session to the process. The session first gets the
// output in the buffer, then the new output. It ends when the process
// exits, when another session attaches, or when the session ends. The end
// of a session does not stop the process.
func (s *server) attach(parent context.Context, k *keptProcess, stream *provider.Stream, rows, cols uint16) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	me := &viewer{detach: cancel}
	k.mu.Lock()
	if k.viewer != nil {
		k.viewer.detach(errDetached)
	}
	k.viewer = me
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		if k.viewer == me {
			k.viewer = nil
		}
		k.mu.Unlock()
	}()
	// A pending write to the session waits for credit. Abort releases it.
	stop := context.AfterFunc(ctx, func() { stream.Abort(context.Cause(ctx)) })
	defer stop()

	if k.terminal != nil && rows > 0 && cols > 0 {
		if err := k.resize(rows, cols, true); err != nil && !k.done() {
			return err
		}
	}

	go func() {
		reader := execadapter.NewReader(stream)
		for {
			record, err := reader.Next()
			// The end of session input does not close the input of the
			// process. A later session can still write to it.
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return
			}
			if err != nil {
				cancel(err)
				return
			}
			switch record.Type {
			case "stdin":
				err = k.write(record.Data)
			case "resize":
				if k.terminal == nil || record.Rows == 0 || record.Cols == 0 {
					err = provider.ErrInvalidRequest
				} else {
					err = k.resize(record.Rows, record.Cols, false)
				}
			default:
				err = provider.ErrInvalidRequest
			}
			if err != nil {
				if !k.done() && !inputGone(err) {
					cancel(err)
				}
				return
			}
		}
	}()

	encoder := json.NewEncoder(stream)
	send := func(record execadapter.Record) error {
		if err := encoder.Encode(record); err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return err
		}
		return nil
	}
	var at int64
	for {
		k.mu.Lock()
		chunks, next := k.output.since(at)
		exited, exit, changed := k.exited, k.exit, k.changed
		k.mu.Unlock()
		for _, chunk := range chunks {
			if err := send(execadapter.Record{Type: chunk.channel, Data: chunk.data}); err != nil {
				return err
			}
		}
		at = next
		if exited {
			if err := send(execadapter.Record{Type: "exit", Exit: exit}); err != nil {
				return err
			}
			// A session showed the exit status, so the name is free again.
			s.forget(k)
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// find returns the kept process with the name, or nil.
func (s *server) find(name string) *keptProcess {
	s.kept.mu.Lock()
	defer s.kept.mu.Unlock()
	return s.kept.byName[name]
}

// forget removes the process from the names, if the name still holds it.
func (s *server) forget(k *keptProcess) {
	s.kept.mu.Lock()
	defer s.kept.mu.Unlock()
	if s.kept.byName[k.name] == k {
		delete(s.kept.byName, k.name)
	}
}

// kill stops the process group of a kept process and removes its name.
func (s *server) kill(ctx context.Context, name string) (map[string]any, error) {
	k := s.find(name)
	if k == nil {
		return nil, provider.Error("not_found")
	}
	state := "exited"
	if !k.done() {
		state = "killed"
		killGroup(k.process)
	}
	select {
	case <-k.ended:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.forget(k)
	k.mu.Lock()
	exit := k.exit
	k.mu.Unlock()
	return map[string]any{"name": name, "state": state, "exit": exit}, nil
}

// list describes each kept process, as a table.
func (s *server) list() map[string]any {
	s.kept.mu.Lock()
	all := make([]*keptProcess, 0, len(s.kept.byName))
	for _, k := range s.kept.byName {
		all = append(all, k)
	}
	s.kept.mu.Unlock()
	slices.SortFunc(all, func(a, b *keptProcess) int { return strings.Compare(a.name, b.name) })
	rows := [][]any{}
	for _, k := range all {
		k.mu.Lock()
		state, exit := "running", any(nil)
		if k.exited {
			state, exit = "exited", k.exit
		}
		rows = append(rows, []any{k.name, state, exit, k.viewer != nil, k.terminal != nil,
			k.started.UTC().Format("2006-01-02T15:04:05Z"), k.owner})
		k.mu.Unlock()
	}
	return map[string]any{
		"columns": []string{"name", "state", "exit", "attached", "pty", "started_at", "owner"},
		"rows":    rows,
	}
}

// stopKept stops each kept process. The provider calls it when it stops.
func (s *server) stopKept() {
	s.kept.mu.Lock()
	s.kept.stopped = true
	all := make([]*keptProcess, 0, len(s.kept.byName))
	for _, k := range s.kept.byName {
		all = append(all, k)
	}
	s.kept.mu.Unlock()
	for _, k := range all {
		if !k.done() {
			killGroup(k.process)
		}
	}
	for _, k := range all {
		<-k.ended
	}
}

func (k *keptProcess) done() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.exited
}

// notify wakes each session that waits for output. The caller holds k.mu.
func (k *keptProcess) notify() {
	close(k.changed)
	k.changed = make(chan struct{})
}

func (k *keptProcess) write(data []byte) error {
	k.inputMu.Lock()
	defer k.inputMu.Unlock()
	_, err := k.input.Write(data)
	if errors.Is(err, os.ErrClosed) {
		err = syscall.EPIPE
	}
	return err
}

// resize sets the size of the terminal. On attach, the process must draw its
// screen again. If the size does not change, the kernel sends no SIGWINCH, so
// the provider sends it.
func (k *keptProcess) resize(rows, cols uint16, redraw bool) error {
	k.inputMu.Lock()
	defer k.inputMu.Unlock()
	conn, err := k.terminal.SyscallConn()
	if err != nil {
		return err
	}
	var size *unix.Winsize
	controlErr := conn.Control(func(fd uintptr) {
		size, err = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	})
	if controlErr != nil {
		return controlErr
	}
	if err != nil {
		return err
	}
	if size.Row == rows && size.Col == cols {
		if redraw {
			_ = syscall.Kill(-k.process.Process.Pid, syscall.SIGWINCH)
		}
		return nil
	}
	return setsize(k.terminal, rows, cols)
}

// keptOutput adds the output of one channel to the buffer.
type keptOutput struct {
	k       *keptProcess
	channel string
}

func (w keptOutput) Write(p []byte) (int, error) {
	w.k.mu.Lock()
	defer w.k.mu.Unlock()
	w.k.output.add(w.channel, p)
	w.k.notify()
	return len(p), nil
}

// outputBuffer keeps the newest output of a kept process, at most limit
// bytes. An offset counts each byte from the start of the process.
type outputBuffer struct {
	limit  int
	size   int
	chunks []outputChunk
	first  int64
	next   int64
}

type outputChunk struct {
	channel string
	data    []byte
}

// add appends the output, and drops the oldest bytes above the limit.
func (b *outputBuffer) add(channel string, data []byte) {
	for len(data) > 0 {
		n := min(len(data), 4096, b.limit)
		b.chunks = append(b.chunks, outputChunk{channel, bytes.Clone(data[:n])})
		b.size += n
		b.next += int64(n)
		data = data[n:]
		for b.size > b.limit {
			over := b.size - b.limit
			if oldest := len(b.chunks[0].data); over >= oldest {
				b.chunks = b.chunks[1:]
				over = oldest
			} else {
				b.chunks[0].data = b.chunks[0].data[over:]
			}
			b.size -= over
			b.first += int64(over)
		}
	}
}

// since returns the output from offset at, and the offset after it. If the
// buffer dropped output after at, it starts with its oldest output.
func (b *outputBuffer) since(at int64) ([]outputChunk, int64) {
	var out []outputChunk
	offset := b.first
	for _, chunk := range b.chunks {
		end := offset + int64(len(chunk.data))
		if end > at {
			skip := max(0, at-offset)
			out = append(out, outputChunk{chunk.channel, chunk.data[skip:]})
		}
		offset = end
	}
	return out, b.next
}
