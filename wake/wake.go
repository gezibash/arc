// Package wake wakes citizens whose machines pause.
//
// A caller keeps one wake hook for each such citizen in wake.toml, in the
// directory of ARC. The hook has the role that ProxyCommand has in
// ~/.ssh/config: it runs a local program that wakes the machine, and exits 0
// when the citizen is ready.
//
//	[wake."<64 characters of hex>"]
//	kind = "command"
//	argv = ["sprite", "exec", "-s", "arc", "--", "/home/sprite/exec-provider/citizen/citizen-up"]
//
// Before a request to the citizen, the caller runs the hook, unless the
// citizen answered or woke less than Fresh ago. See docs/exec/SPEC.md,
// section 10.
package wake

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gezibash/arc/identity"
	"github.com/pelletier/go-toml/v2"
)

// FileName is the wake configuration in the directory of ARC.
const FileName = "wake.toml"

// StateDirName is the directory, beside FileName, that keeps the time of the
// last answer of each citizen.
const StateDirName = "wake"

const (
	// DefaultTimeout bounds one wake, the hook included.
	DefaultTimeout = 30 * time.Second
	// Fresh is how long an answer proves that a citizen is awake. A citizen
	// releases its lease 60 seconds after its last command, so an answer
	// younger than this comes from a machine that did not pause.
	Fresh = 30 * time.Second
	// MaxFileBytes caps the wake configuration.
	MaxFileBytes = 64 * 1024
)

// The errors of a wake. Their text is the code that the exec spec names.
var (
	// ErrFailed reports a hook that did not start, or that exited with a
	// status other than 0.
	ErrFailed = errors.New("wake_failed")
	// ErrTimeout reports a hook that did not end in time.
	ErrTimeout = errors.New("wake_timeout")
)

// stderrLimit caps the output of a hook that an error keeps.
const stderrLimit = 4096

// Hook is one entry of the wake configuration.
type Hook struct {
	Kind string   `toml:"kind"`
	Argv []string `toml:"argv"`
}

// Waker runs the wake hooks of one caller.
type Waker struct {
	// Timeout bounds one wake. Zero means DefaultTimeout.
	Timeout time.Duration

	hooks    map[string][]string
	stateDir string

	mu    sync.Mutex
	awake map[string]time.Time
	locks map[string]*sync.Mutex
}

// Load reads the wake configuration at path. A missing file gives a waker
// without hooks. The waker keeps the time of the last answer of each citizen
// in stateDir, so that the next process sees it too.
func Load(path, stateDir string) (*Waker, error) {
	waker := &Waker{
		hooks:    map[string][]string{},
		stateDir: stateDir,
		awake:    map[string]time.Time{},
		locks:    map[string]*sync.Mutex{},
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return waker, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("wake: %s is over %d bytes", path, MaxFileBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var held struct {
		Wake map[string]Hook `toml:"wake"`
	}
	if err := toml.Unmarshal(data, &held); err != nil {
		return nil, fmt.Errorf("wake: %s: %w", path, err)
	}

	home, _ := os.UserHomeDir()
	for key, hook := range held.Wake {
		citizen, err := hex.DecodeString(key)
		if err != nil || len(citizen) != identity.SeedBytes {
			return nil, fmt.Errorf("wake: %s: %q is not a public key of 64 characters of hex", path, key)
		}
		if hook.Kind != "command" {
			return nil, fmt.Errorf("wake: %s: the hook of %s has the kind %q, and only \"command\" works", path, identity.Name(citizen), hook.Kind)
		}
		if len(hook.Argv) == 0 || hook.Argv[0] == "" {
			return nil, fmt.Errorf("wake: %s: the hook of %s has no argv", path, identity.Name(citizen))
		}

		argv := make([]string, len(hook.Argv))
		for index, part := range hook.Argv {
			argv[index] = expandHome(part, home)
		}
		waker.hooks[string(citizen)] = argv
	}
	return waker, nil
}

// Has says whether the caller keeps a hook for the citizen.
func (w *Waker) Has(citizen []byte) bool {
	_, ok := w.hooks[string(citizen)]
	return ok
}

// Wake makes the citizen ready. Without a hook it returns at once, and the
// request goes out as it would without a waker. With a hook, it runs the hook
// unless the citizen answered or woke less than Fresh ago. Exit status 0 of
// the hook means that the citizen is ready.
func (w *Waker) Wake(ctx context.Context, citizen []byte) error {
	argv, ok := w.hooks[string(citizen)]
	if !ok {
		return nil
	}

	// One wake at a time for each citizen. A second request waits for the
	// first, and then finds the citizen fresh.
	lock := w.lock(citizen)
	lock.Lock()
	defer lock.Unlock()

	if w.fresh(citizen) {
		return nil
	}

	if err := w.run(ctx, citizen, argv); err != nil {
		return err
	}
	w.Answered(citizen)
	return nil
}

// Answered records that the citizen answered now. A citizen without a hook
// leaves no record.
func (w *Waker) Answered(citizen []byte) {
	if !w.Has(citizen) {
		return
	}

	now := time.Now()
	w.mu.Lock()
	w.awake[string(citizen)] = now
	w.mu.Unlock()

	// The record is a help, and never a condition: a caller that cannot
	// write it wakes the citizen again next time.
	if w.stateDir == "" || os.MkdirAll(w.stateDir, 0o700) != nil {
		return
	}
	_ = os.WriteFile(w.stamp(citizen), []byte(now.UTC().Format(time.RFC3339Nano)), 0o600)
}

func (w *Waker) fresh(citizen []byte) bool {
	w.mu.Lock()
	last, ok := w.awake[string(citizen)]
	w.mu.Unlock()

	if !ok && w.stateDir != "" {
		data, err := os.ReadFile(w.stamp(citizen))
		if err == nil {
			last, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
			ok = err == nil
		}
	}

	age := time.Since(last)
	return ok && age >= 0 && age < Fresh
}

func (w *Waker) run(ctx context.Context, citizen []byte, argv []string) error {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name := identity.Name(citizen)
	stderr := &tail{limit: stderrLimit}

	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stderr = stderr
	// The hook leads its own process group, so a hook that runs too long
	// stops together with every program that it started.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	// A hook may leave a program behind that holds its standard error open.
	// The exit of the hook decides, and the wait for its output ends soon.
	command.WaitDelay = time.Second

	// Wait gives ErrWaitDelay only for a hook that exited 0.
	err := command.Run()
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: the hook of %s did not end in %s", ErrTimeout, name, timeout)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%w: the hook of %s exited with status %d%s", ErrFailed, name, exit.ExitCode(), stderr.lastLine())
	}
	return fmt.Errorf("%w: the hook of %s did not start: %v", ErrFailed, name, err)
}

func (w *Waker) lock(citizen []byte) *sync.Mutex {
	w.mu.Lock()
	defer w.mu.Unlock()

	held, ok := w.locks[string(citizen)]
	if !ok {
		held = &sync.Mutex{}
		w.locks[string(citizen)] = held
	}
	return held
}

func (w *Waker) stamp(citizen []byte) string {
	return filepath.Join(w.stateDir, hex.EncodeToString(citizen))
}

// expandHome reads a leading ~ as the home directory, as a shell would.
func expandHome(part, home string) string {
	switch {
	case home == "":
		return part
	case part == "~":
		return home
	case strings.HasPrefix(part, "~/"):
		return filepath.Join(home, part[2:])
	}
	return part
}

// tail keeps the last bytes that a hook writes.
type tail struct {
	limit int
	mu    sync.Mutex
	data  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.data = append(t.data, p...)
	if len(t.data) > t.limit {
		t.data = t.data[len(t.data)-t.limit:]
	}
	return len(p), nil
}

// lastLine gives the last line that the hook wrote, as the end of an error.
func (t *tail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	lines := strings.Split(strings.TrimSpace(string(t.data)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return ""
	}
	return ": " + last
}
