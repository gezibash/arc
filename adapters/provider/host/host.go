// Package host runs a provider program, and speaks to it.
//
// A provider program reads newline delimited JSON on its standard input and
// writes it on its standard output. It knows nothing about relays, sessions
// or events. Both the ARC citizen and the delivery layer host providers this
// way, so a provider runs unchanged under either.
package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gezibash/arc/core/wire"
)

// MaxLineBytes caps one line from the provider. A provider that never writes
// a newline must not fill the memory of the citizen.
const MaxLineBytes = 64 * 1024 * 1024

// StopGrace is how long a provider has to end by itself after its input
// closes. A provider that holds files or locks releases them in that time.
const StopGrace = 5 * time.Second

// Process is one running provider program.
type Process struct {
	command   *exec.Cmd
	stdin     *os.File
	lines     chan wire.Event
	writeGate chan struct{}
	stopped   chan struct{}
	log       *slog.Logger

	mu     sync.Mutex
	closed bool
}

// Start starts a provider program, with the environment that a provider
// expects added to this process's own. The program runs in the directory
// cwd. If cwd is empty, it runs in the directory of this process.
func Start(path string, args []string, cwd string, environment []string, log *slog.Logger) (*Process, error) {
	command := exec.Command(path, args...)
	command.Dir = cwd
	// Environ sets PWD to cwd, so PWD names the directory of the program.
	command.Env = append(command.Environ(), environment...)

	pipe, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdin, ok := pipe.(*os.File)
	if !ok {
		return nil, errors.New("host: the standard input of the provider is not a file")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("host: the provider did not start: %w", err)
	}

	provider := &Process{
		command:   command,
		stdin:     stdin,
		lines:     make(chan wire.Event, 64),
		log:       log,
		writeGate: make(chan struct{}, 1), stopped: make(chan struct{}),
	}

	go provider.read(stdout)
	go provider.report(stderr)
	return provider, nil
}

// Send writes one event to the provider.
func (r *Process) Send(ctx context.Context, event wire.Event) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	select {
	case r.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.stopped:
		return errors.New("host: the provider is not running")
	}
	defer func() { <-r.writeGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := r.stdin.SetWriteDeadline(deadline); err != nil {
		return err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = r.stdin.SetWriteDeadline(time.Now()); close(interrupted) })
	n, err := r.stdin.Write(append(line, '\n'))
	if !stop() {
		<-interrupted
	}
	_ = r.stdin.SetWriteDeadline(time.Time{})
	if err != nil && n > 0 {
		// The next line cannot safely follow a partial JSON document.
		_ = r.stdin.Close()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (r *Process) read(stdout io.Reader) {
	defer close(r.lines)
	reader := bufio.NewReaderSize(stdout, 64*1024)
	for {
		line, err := wire.ReadLine(reader, MaxLineBytes)
		if errors.Is(err, wire.ErrLineTooLong) {
			r.log.Warn("the provider wrote a line over the limit")
			continue
		}
		if err != nil {
			return
		}
		if len(line) == 0 {
			continue
		}
		var answer wire.Event
		if err := json.Unmarshal(line, &answer); err != nil {
			r.log.Warn("the provider wrote a line that is not JSON", "bytes", len(line))
			continue
		}
		select {
		case r.lines <- answer:
		case <-r.stopped:
			return
		}
	}
}

// report passes the standard error of the provider to the log.
func (r *Process) report(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		r.log.Info("provider", "message", scanner.Text())
	}
}

// Lines returns each answer of the provider. The channel closes when the
// provider stops.
func (r *Process) Lines() <-chan wire.Event { return r.lines }

// Stop closes the input of the provider and waits for it to end. A provider
// that is still running after StopGrace is killed.
func (r *Process) Stop() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.stopped)
	// A partial write in Send can have closed the input already.
	if err := r.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		r.log.Warn("the input of the provider did not close", "error", err)
	}
	r.mu.Unlock()

	ended := make(chan error, 1)
	go func() { ended <- r.command.Wait() }()

	select {
	case err := <-ended:
		return err
	case <-time.After(StopGrace):
	}

	r.log.Warn("the provider did not end after its input closed, so it is killed", "grace", StopGrace)
	if r.command.Process != nil {
		_ = r.command.Process.Kill()
	}
	return <-ended
}
