package citizen

import (
	"strings"
	"testing"
)

// A relay chooses the text of a NOTICE, so arc prints it on one line,
// escaped, and cut to 300 characters of output.
func TestNoticeIsCutAndEscaped(t *testing.T) {
	if got, want := notice("arc-ring: \x1b[2Jfail\narc-ring: forged"), `arc-ring: \x1b[2Jfail\narc-ring: forged`; got != want {
		t.Errorf("notice = %q, want %q", got, want)
	}
	// 299 characters, then ESC, which takes 4 characters escaped: it does
	// not fit, so the notice ends after the 299.
	long := strings.Repeat("x", 299) + "\x1b" + "tail"
	if got, want := notice(long), strings.Repeat("x", 299)+"\u2026"; got != want {
		t.Errorf("notice of %d characters = %q, want 299 characters and an ellipsis", len(long), got)
	}
	// 296 characters, then ESC: 300 characters of output, all of it shown.
	exact := strings.Repeat("x", 296) + "\x1b"
	if got, want := notice(exact), strings.Repeat("x", 296)+`\x1b`; got != want {
		t.Errorf("notice of 300 characters of output = %q, want it whole", got)
	}
}
