// Package wake runs the wake flow of a caller: it wakes citizens whose
// machines pause, and it refuses a request to a citizen that is not there.
//
// A caller keeps one wake hook for each citizen that pauses, in wake.toml in
// the directory of ARC. The hook has the role that ProxyCommand has in
// ~/.ssh/config: it runs a local program that wakes the machine, and exits 0
// when the citizen is ready.
//
//	[wake."<64 characters of hex>"]
//	kind = "command"
//	argv = ["sprite", "exec", "-s", "arc", "--", "/home/sprite/exec-provider/citizen/citizen-up"]
//
// Before a request to a citizen with a hook, the caller runs the hook, unless
// the citizen answered or woke less than Fresh ago. A citizen without a hook
// must have a current announcement on the relay. See docs/exec/SPEC.md,
// sections 9 and 10.
package wake

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// last answer of each citizen with a hook.
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

// The errors of the wake flow. Their text is the code that the exec spec
// names.
var (
	// ErrFailed reports a hook that did not start, that exited with a status
	// other than 0, or that this caller cannot run.
	ErrFailed = errors.New("wake_failed")
	// ErrTimeout reports a hook that did not end in time.
	ErrTimeout = errors.New("wake_timeout")
	// ErrPeerOffline reports a citizen without a hook that has no current
	// announcement on the relay.
	ErrPeerOffline = errors.New("peer_offline")
)

// The presence states of a citizen, as a caller sees it (spec section 9).
const (
	// Online: the relay has a current announcement of the citizen.
	Online = "online"
	// Asleep: no current announcement, and the caller keeps a hook.
	Asleep = "asleep"
	// Offline: no current announcement, and no hook.
	Offline = "offline"
)

// stderrLimit caps the output of a hook that an error keeps.
const stderrLimit = 4096

// Hook is one entry of the wake configuration.
type Hook struct {
	Kind string   `toml:"kind"`
	Argv []string `toml:"argv"`
}

// Waker runs the wake flow of one caller.
type Waker struct {
	// Timeout bounds one wake. Zero means DefaultTimeout.
	Timeout time.Duration

	// problem makes the whole configuration unusable, for example a file
	// that is not TOML. Each wake reports it.
	problem error
	// hooks holds the argv of each hook, and broken the reason that the
	// hook of a citizen cannot run.
	hooks    map[string][]string
	broken   map[string]error
	stateDir string

	mu    sync.Mutex
	awake map[string]time.Time
	locks map[string]*sync.Mutex
}

// Load reads the wake configuration at path. A missing file gives a waker
// without hooks. The waker keeps the time of the last answer of each citizen
// with a hook in stateDir, so that the next process sees it too.
//
// Load always gives a waker. A configuration that cannot work fails the
// requests that need it, and nothing else: a hook of an unknown kind fails
// the requests to its citizen, and a file that is not valid fails every
// request, because it may hold the hook of any citizen.
func Load(path, stateDir string) *Waker {
	waker := &Waker{
		hooks:    map[string][]string{},
		broken:   map[string]error{},
		stateDir: stateDir,
		awake:    map[string]time.Time{},
		locks:    map[string]*sync.Mutex{},
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return waker
	}
	if err == nil && info.Size() > MaxFileBytes {
		err = fmt.Errorf("the file is over %d bytes", MaxFileBytes)
	}

	var data []byte
	if err == nil {
		data, err = os.ReadFile(path)
	}

	var held struct {
		Wake map[string]Hook `toml:"wake"`
	}
	if err == nil {
		err = toml.Unmarshal(data, &held)
	}

	if err == nil {
		for key := range held.Wake {
			citizen, decodeErr := hex.DecodeString(key)
			if decodeErr != nil || len(citizen) != identity.SeedBytes {
				err = fmt.Errorf("%q is not a public key of 64 characters of hex", key)
				break
			}
		}
	}
	if err != nil {
		waker.problem = fmt.Errorf("wake: %s: %w", path, err)
		return waker
	}

	home, _ := os.UserHomeDir()
	for key, hook := range held.Wake {
		citizen, _ := hex.DecodeString(key)
		name := identity.Name(citizen)

		switch {
		case hook.Kind != "command":
			waker.broken[string(citizen)] = fmt.Errorf("%w: the hook of %s has the kind %q, and this arc runs only the kind \"command\"", ErrFailed, name, hook.Kind)
		case len(hook.Argv) == 0 || hook.Argv[0] == "":
			waker.broken[string(citizen)] = fmt.Errorf("%w: the hook of %s has no argv", ErrFailed, name)
		default:
			argv := make([]string, len(hook.Argv))
			for index, part := range hook.Argv {
				argv[index] = expandHome(part, home)
			}
			waker.hooks[string(citizen)] = argv
		}
	}
	return waker
}

// Has says whether the caller keeps a hook for the citizen. A hook that
// cannot run counts, because the citizen is still one that pauses.
func (w *Waker) Has(citizen []byte) bool {
	_, ok := w.hooks[string(citizen)]
	_, broken := w.broken[string(citizen)]
	return ok || broken
}

// Citizens gives the citizens with a hook, in the order of their keys.
func (w *Waker) Citizens() [][]byte {
	keys := make([]string, 0, len(w.hooks)+len(w.broken))
	for key := range w.hooks {
		keys = append(keys, key)
	}
	for key := range w.broken {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	citizens := make([][]byte, len(keys))
	for index, key := range keys {
		citizens[index] = []byte(key)
	}
	return citizens
}

// State gives the presence state of a citizen: Online when the relay has a
// current announcement of it, Asleep when it has none and the caller keeps a
// hook, and Offline otherwise.
func (w *Waker) State(citizen []byte, announced bool) string {
	switch {
	case announced:
		return Online
	case w.Has(citizen):
		return Asleep
	}
	return Offline
}

// Wake makes the citizen ready for a request (spec section 10.2).
//
// A citizen with a hook: Wake runs the hook, unless the citizen answered or
// woke less than Fresh ago. Exit status 0 of the hook means that the citizen
// is ready.
//
// A citizen without a hook: Wake asks online whether the relay has a current
// announcement of it, and gives ErrPeerOffline when the relay has none. A
// citizen that answered less than Fresh ago needs no question. When online
// is nil or cannot answer, the request goes out as it would without a waker.
func (w *Waker) Wake(ctx context.Context, citizen []byte, online func(context.Context, []byte) (bool, error)) error {
	if w.problem != nil {
		return w.problem
	}
	if err, broken := w.broken[string(citizen)]; broken {
		return err
	}

	argv, ok := w.hooks[string(citizen)]
	if !ok {
		if online == nil || w.fresh(citizen) {
			return nil
		}
		announced, err := online(ctx, citizen)
		if err == nil && !announced {
			return fmt.Errorf("%w: %s has no current announcement on the relay, and this machine keeps no wake hook for it", ErrPeerOffline, identity.Name(citizen))
		}
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

// Answered records that the citizen answered now. Only a citizen with a hook
// leaves a record for the next process.
func (w *Waker) Answered(citizen []byte) {
	now := time.Now()
	w.mu.Lock()
	w.awake[string(citizen)] = now
	w.mu.Unlock()

	// The record is a help, and never a condition: a caller that cannot
	// write it wakes the citizen again next time.
	if !w.Has(citizen) || w.stateDir == "" || os.MkdirAll(w.stateDir, 0o700) != nil {
		return
	}
	_ = os.WriteFile(w.stamp(citizen), []byte(now.UTC().Format(time.RFC3339Nano)), 0o600)
}

func (w *Waker) fresh(citizen []byte) bool {
	w.mu.Lock()
	last, ok := w.awake[string(citizen)]
	w.mu.Unlock()

	if !ok && w.Has(citizen) && w.stateDir != "" {
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

	// The request may end sooner than the wake limit. The error then names
	// the time that the hook had.
	limit := timeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < limit {
		limit = time.Until(deadline)
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
		return fmt.Errorf("%w: the hook of %s did not end in %s", ErrTimeout, name, limit.Round(100*time.Millisecond))
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
