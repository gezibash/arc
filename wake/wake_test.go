package wake_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/wake"
)

// citizen makes the public key of a citizen.
func citizen(t *testing.T) []byte {
	t.Helper()

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return me.PublicKey
}

// configure writes a wake configuration, and loads it.
func configure(t *testing.T, text string) (*wake.Waker, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, wake.FileName)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	waker, err := wake.Load(path, filepath.Join(dir, wake.StateDirName))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return waker, dir
}

// hook gives the configuration of one hook that runs a shell script.
func hook(key []byte, script string) string {
	return fmt.Sprintf("[wake.%q]\nkind = \"command\"\nargv = [\"sh\", \"-c\", %q]\n", hex.EncodeToString(key), script)
}

// runs counts the lines that a hook wrote to its marker.
func runs(t *testing.T, marker string) int {
	t.Helper()

	data, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func TestAMissingFileGivesNoHooks(t *testing.T) {
	dir := t.TempDir()
	waker, err := wake.Load(filepath.Join(dir, wake.FileName), filepath.Join(dir, wake.StateDirName))
	if err != nil {
		t.Fatal(err)
	}

	key := citizen(t)
	if waker.Has(key) {
		t.Error("a waker without a file has a hook")
	}
	if err := waker.Wake(context.Background(), key); err != nil {
		t.Errorf("a citizen without a hook: %v", err)
	}
}

func TestLoadReadsTheHomeDirectoryInArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	key := citizen(t)
	marker := filepath.Join(home, "woke")
	script := filepath.Join(home, "bin", "wake-me")
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho woke >> \"$HOME/woke\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	waker, _ := configure(t, fmt.Sprintf("[wake.%q]\nkind = \"command\"\nargv = [\"~/bin/wake-me\"]\n", hex.EncodeToString(key)))
	if err := waker.Wake(context.Background(), key); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if runs(t, marker) != 1 {
		t.Errorf("the hook at ~/bin did not run")
	}
}

func TestLoadRefusesABadConfiguration(t *testing.T) {
	key := hex.EncodeToString(citizen(t))

	for name, text := range map[string]string{
		"a key that is not hex": "[wake.\"not-a-key\"]\nkind = \"command\"\nargv = [\"true\"]\n",
		"a short key":           "[wake.\"abcd\"]\nkind = \"command\"\nargv = [\"true\"]\n",
		"another kind":          fmt.Sprintf("[wake.%q]\nkind = \"https\"\nargv = [\"true\"]\n", key),
		"no argv":               fmt.Sprintf("[wake.%q]\nkind = \"command\"\n", key),
		"not TOML":              "[wake\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, wake.FileName)
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := wake.Load(path, dir); err == nil {
				t.Error("the configuration loaded")
			}
		})
	}
}

func TestWakeRunsTheHookOnceAndThenFindsTheCitizenFresh(t *testing.T) {
	key := citizen(t)
	marker := filepath.Join(t.TempDir(), "woke")

	waker, _ := configure(t, hook(key, "echo woke >> '"+marker+"'"))
	if !waker.Has(key) {
		t.Fatal("the waker has no hook for the citizen")
	}

	for range 3 {
		if err := waker.Wake(context.Background(), key); err != nil {
			t.Fatalf("wake: %v", err)
		}
	}
	if got := runs(t, marker); got != 1 {
		t.Errorf("the hook ran %d times, and a woken citizen is fresh", got)
	}
}

func TestAnAnswerSkipsTheHook(t *testing.T) {
	key := citizen(t)
	marker := filepath.Join(t.TempDir(), "woke")

	waker, _ := configure(t, hook(key, "echo woke >> '"+marker+"'"))
	waker.Answered(key)

	if err := waker.Wake(context.Background(), key); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if runs(t, marker) != 0 {
		t.Error("the hook ran for a citizen that answered a moment ago")
	}
}

// A second process reads the answer that the first one recorded.
func TestTheNextProcessSeesTheAnswer(t *testing.T) {
	key := citizen(t)
	marker := filepath.Join(t.TempDir(), "woke")
	text := hook(key, "echo woke >> '"+marker+"'")

	first, dir := configure(t, text)
	first.Answered(key)

	second, err := wake.Load(filepath.Join(dir, wake.FileName), filepath.Join(dir, wake.StateDirName))
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Wake(context.Background(), key); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if runs(t, marker) != 0 {
		t.Error("the second process ran the hook, and the first recorded a fresh answer")
	}
}

func TestAnOldAnswerRunsTheHook(t *testing.T) {
	key := citizen(t)
	marker := filepath.Join(t.TempDir(), "woke")

	waker, dir := configure(t, hook(key, "echo woke >> '"+marker+"'"))

	old := time.Now().Add(-2 * wake.Fresh).UTC().Format(time.RFC3339Nano)
	state := filepath.Join(dir, wake.StateDirName)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, hex.EncodeToString(key)), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := waker.Wake(context.Background(), key); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if runs(t, marker) != 1 {
		t.Error("the hook did not run after an old answer")
	}
}

func TestAHookThatFailsGivesWakeFailed(t *testing.T) {
	key := citizen(t)
	waker, _ := configure(t, hook(key, "echo 'the sprite is gone' >&2; exit 3"))

	err := waker.Wake(context.Background(), key)
	if !errors.Is(err, wake.ErrFailed) {
		t.Fatalf("err = %v, and it must be wake_failed", err)
	}
	for _, want := range []string{"wake_failed", identity.Name(key), "status 3", "the sprite is gone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}

	// A failed wake leaves the citizen asleep.
	if err := waker.Wake(context.Background(), key); !errors.Is(err, wake.ErrFailed) {
		t.Errorf("the second wake: %v", err)
	}
}

func TestAHookThatDoesNotStartGivesWakeFailed(t *testing.T) {
	key := citizen(t)
	waker, _ := configure(t, fmt.Sprintf("[wake.%q]\nkind = \"command\"\nargv = [\"/no/such/program\"]\n", hex.EncodeToString(key)))

	if err := waker.Wake(context.Background(), key); !errors.Is(err, wake.ErrFailed) {
		t.Errorf("err = %v, and it must be wake_failed", err)
	}
}

// A slow hook stops at the timeout, together with the programs it started.
func TestASlowHookGivesWakeTimeout(t *testing.T) {
	key := citizen(t)
	pidFile := filepath.Join(t.TempDir(), "child")

	waker, _ := configure(t, hook(key, "sleep 30 & echo $! > '"+pidFile+"'; wait"))
	waker.Timeout = 300 * time.Millisecond

	started := time.Now()
	err := waker.Wake(context.Background(), key)
	if !errors.Is(err, wake.ErrTimeout) {
		t.Fatalf("err = %v, and it must be wake_timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the timeout took %s", elapsed)
	}

	child := readPID(t, pidFile)
	deadline := time.Now().Add(2 * time.Second)
	for alive(child) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(child) {
		syscall.Kill(child, syscall.SIGKILL)
		t.Error("the program that the hook started outlived the timeout")
	}
}

// A hook may start the citizen and leave it running. Its exit decides.
func TestAHookThatLeavesAProgramBehindSucceeds(t *testing.T) {
	key := citizen(t)
	pidFile := filepath.Join(t.TempDir(), "child")

	waker, _ := configure(t, hook(key, "sleep 30 & echo $! > '"+pidFile+"'"))

	started := time.Now()
	if err := waker.Wake(context.Background(), key); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the wake took %s", elapsed)
	}

	child := readPID(t, pidFile)
	if !alive(child) {
		t.Error("the program that the hook left behind stopped")
	}
	syscall.Kill(child, syscall.SIGKILL)
}

func TestTwoRequestsAtOnceRunTheHookOnce(t *testing.T) {
	key := citizen(t)
	marker := filepath.Join(t.TempDir(), "woke")

	waker, _ := configure(t, hook(key, "sleep 0.2; echo woke >> '"+marker+"'"))

	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := waker.Wake(context.Background(), key); err != nil {
				t.Errorf("wake: %v", err)
			}
		}()
	}
	group.Wait()

	if got := runs(t, marker); got != 1 {
		t.Errorf("the hook ran %d times", got)
	}
}

func TestAnAnswerFromACitizenWithoutAHookLeavesNoRecord(t *testing.T) {
	waker, dir := configure(t, "")
	waker.Answered(citizen(t))

	if _, err := os.Stat(filepath.Join(dir, wake.StateDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the waker wrote state for a citizen without a hook: %v", err)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()

	var data []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		if len(strings.TrimSpace(string(data))) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("the hook wrote no process id: %q", data)
	}
	return pid
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
