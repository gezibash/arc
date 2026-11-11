package citizen

import (
	"bytes"
	"sync"
	"testing"

	"github.com/gezibash/arc/sdk/terminal"
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

// A NOTICE of a relay reaches the screen with its URL and its text each
// escaped once: a second escape doubles each backslash (#464), and none
// lets a terminal escape through. The whole line is checked.
func TestANoticeLineIsEscapedOnce(t *testing.T) {
	var out bytes.Buffer
	s := &Session{Errors: &out}
	url := "ws://relay\x1b[31m\u202e\n.example"
	text := "arc-ring: rings fail\x1b[2K\u202eevil\nsecond line"
	s.printNotice(url, text)
	want := terminal.EscapeLine(url) + ": " + terminal.EscapeLineCut(text, noticeLimit) + "\n"
	if out.String() != want {
		t.Errorf("the notice line is %q, want %q", out.String(), want)
	}
}
