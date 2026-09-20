// Package citizen runs one serving citizen: it connects to a relay, offers
// its capability, and passes each request to the provider program.
//
// This is what "arc serve" does. The provider program speaks newline
// delimited JSON on its standard input and its standard output, and knows
// nothing about relays, sessions or packets.
package citizen

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// MaxLineBytes caps one line from the provider. A provider that never writes
// a newline must not fill the memory of the citizen.
const MaxLineBytes = 64 * 1024 * 1024

// runtime is the provider program.
type runtime struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   chan map[string]any
	log     *slog.Logger

	mu     sync.Mutex
	closed bool
}

// startRuntime starts the provider program with the environment that a
// provider expects.
func startRuntime(path string, args []string, environment []string, log *slog.Logger) (*runtime, error) {
	command := exec.Command(path, args...)
	command.Env = append(os.Environ(), environment...)

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
		return nil, fmt.Errorf("citizen: the provider did not start: %w", err)
	}

	provider := &runtime{
		command: command,
		stdin:   stdin,
		lines:   make(chan map[string]any, 64),
		log:     log,
	}

	go provider.read(stdout)
	go provider.report(stderr)
	return provider, nil
}

// send writes one event to the provider.
func (r *runtime) send(event map[string]any) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return errors.New("citizen: the provider is not running")
	}

	line = append(line, '\n')
	_, err = r.stdin.Write(line)
	return err
}

// read joins the chunks of one line, and passes each answer on. A line over
// the cap goes, and the provider keeps running.
func (r *runtime) read(stdout io.Reader) {
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

func (r *runtime) deliver(line []byte) {
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

// report passes the standard error of the provider to the log of the citizen.
func (r *runtime) report(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		r.log.Info("provider", "message", scanner.Text())
	}
}

// stop closes the input of the provider and waits for it to end.
func (r *runtime) stop() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.stdin.Close()
	r.mu.Unlock()

	if r.command.Process != nil {
		r.command.Process.Kill()
	}
	return r.command.Wait()
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
