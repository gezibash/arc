package citizen

import (
	"strings"
	"testing"
)

// A relay chooses the text of a NOTICE, so arc cuts it to 300 characters
// and escapes it before it prints it.
func TestNoticeIsCutAndEscaped(t *testing.T) {
	if got, want := notice("arc-ring: \x1b[2Jfail"), `arc-ring: \x1b[2Jfail`; got != want {
		t.Errorf("notice = %q, want %q", got, want)
	}
	long := strings.Repeat("x", 299) + "\x1b" + "tail"
	if got, want := notice(long), strings.Repeat("x", 299)+`\x1b`+"\u2026"; got != want {
		t.Errorf("notice of %d characters = %q, want 300 characters, escaped, then an ellipsis", len(long), got)
	}
}
