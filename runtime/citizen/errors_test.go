package citizen

import (
	"bytes"
	"sync"
	"testing"
)

// A watch ends in its own goroutine, and can end after its command. Its
// line must not reach the writer of the command after Close: a test that
// read that writer raced with the line (#438). Under the race build, the
// lines that race with Close also show that each write holds the lock.
func TestNoLineOfTheSessionComesAfterClose(t *testing.T) {
	var out bytes.Buffer
	s := &Session{Errors: &out}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			for range 50 {
				s.warnf("arc: info: a watch of %s ended, closed by %s: %s\n", "ws://a", "the relay", "gone")
			}
		})
	}
	close(start)
	s.Close()
	after := out.Len()
	s.warnf("arc: info: a watch of %s ended, closed by %s: %s\n", "ws://a", "the relay", "gone")
	wg.Wait()
	if out.Len() != after {
		t.Errorf("the session wrote %d bytes after Close, want none", out.Len()-after)
	}
}
