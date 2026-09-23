// Package host runs a provider program, and speaks to it.
//
// A provider program reads newline delimited JSON on its standard input and
// writes it on its standard output. It knows nothing about relays, sessions
// or events. Both the ARC citizen and the delivery layer host providers this
// way, so a provider runs unchanged under either.
package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// MaxLineBytes caps one line from the provider. A provider that never writes
// a newline must not fill the memory of the citizen.
const MaxLineBytes = 64 * 1024 * 1024

// StopGrace is how long a provider has to end by itself after its input
// closes. A provider that holds files or locks releases them in that time.
const StopGrace = 5 * time.Second

// Process is one running provider program.
type Process struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   chan map[string]any
	log     *slog.Logger

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

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
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
		command: command,
		stdin:   stdin,
		lines:   make(chan map[string]any, 64),
		log:     log,
	}

	go provider.read(stdout)
	go provider.report(stderr)
	return provider, nil
}

// Send writes one event to the provider.
func (r *Process) Send(event map[string]any) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errors.New("host: the provider is not running")
	}

	line = append(line, '\n')
	_, err = r.stdin.Write(line)
	return err
}

// read joins the chunks of one line, and passes each answer on. A line over
// the cap goes, and the provider keeps running.
func (r *Process) read(stdout io.Reader) {
	defer close(r.lines)

	reader := bufio.NewReaderSize(stdout, 64*1024)
	var line []byte

	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)

		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > MaxLineBytes {
				r.log.Warn("the provider wrote a line over the limit", "bytes", len(line))
				line = nil
				if err := drain(reader); err != nil {
					return
				}
			}
			continue
		}
		if err != nil {
			if len(line) > 0 {
				r.deliver(line)
			}
			return
		}

		r.deliver(line)
		line = nil
	}
}

func (r *Process) deliver(line []byte) {
	line = []byte(strings.TrimRight(string(line), "\r\n"))
	if len(line) == 0 {
		return
	}

	var answer map[string]any
	if err := json.Unmarshal(line, &answer); err != nil {
		r.log.Warn("the provider wrote a line that is not JSON", "bytes", len(line))
		return
	}
	r.lines <- answer
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
func (r *Process) Lines() <-chan map[string]any { return r.lines }

// Stop closes the input of the provider and waits for it to end. A provider
// that is still running after StopGrace is killed.
func (r *Process) Stop() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.stdin.Close()
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
		r.command.Process.Kill()
	}
	return <-ended
}

func drain(reader *bufio.Reader) error {
	for {
		_, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}
