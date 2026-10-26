package writelock

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test run itself as a writer that holds the lock.
func TestMain(m *testing.M) {
	if dir := os.Getenv("WRITELOCK_HOLD"); dir != "" {
		if _, err := Hold(dir); err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("held\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A write in another process makes the lock busy, and SIGKILL of that
// process frees it: a stopped write holds nothing.
func TestAKilledWriterHoldsNothing(t *testing.T) {
	dir := t.TempDir()
	if busy, err := Busy(dir); err != nil || busy {
		t.Fatalf("with no write: busy %v, %v", busy, err)
	}
	writer := exec.Command(os.Args[0], "-test.run=^$")
	writer.Env = append(os.Environ(), "WRITELOCK_HOLD="+dir)
	out, err := writer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Process.Kill(); _ = writer.Wait() })
	line := make([]byte, 5)
	if _, err := out.Read(line); err != nil || string(line) != "held\n" {
		t.Fatalf("the writer did not take the lock: %q, %v", line, err)
	}
	if busy, err := Busy(dir); err != nil || !busy {
		t.Fatalf("with a write in another process: busy %v, %v", busy, err)
	}
	// A second write shares the lock.
	release, err := Hold(dir)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := writer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Wait()
	if busy, err := Busy(dir); err != nil || busy {
		t.Fatalf("after SIGKILL of the writer: busy %v, %v", busy, err)
	}
}

// A lock that arc cannot use fails closed, with an error that names it and
// says what to do: a lock of mode 000, a symbolic link, whose target arc
// never makes, a folder, and a store folder that arc cannot write.
func TestALockThatArcCannotUseFailsClearly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file of mode 000")
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"mode 000", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0); err != nil {
				t.Fatal(err)
			}
		}, "or remove the file, and arc makes it again"},
		{"a symbolic link", func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}, "is a symbolic link, and arc does not follow it"},
		{"a folder", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "is not a plain file"},
		{"a store folder of mode 555", func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		}, "give the folder"},
	} {
		dir := t.TempDir()
		c.setup(t, dir)
		_, holdErr := Hold(dir)
		_, busyErr := Busy(dir)
		for _, err := range []error{holdErr, busyErr} {
			if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, name)) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v, want an error that names the lock and says %q", c.name, err, c.want)
			}
		}
		if _, err := os.Lstat(filepath.Join(dir, "elsewhere")); err == nil {
			t.Errorf("%s: arc made the target of the link", c.name)
		}
	}
}
