package writelock

import (
	"os"
	"os/exec"
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
