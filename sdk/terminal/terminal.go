// Package terminal makes text from another citizen safe to show on a
// terminal, as docs/clients/SPEC.md, section 3.9, defines.
package terminal

import (
	"fmt"
	"strings"
	"unicode"
)

// Escape writes each character of text that can change a terminal as an
// escape that a person can read: a control character other than a newline
// or a tab becomes \xHH, an embedding, override or isolate of the
// bidirectional algorithm becomes \uHHHH, and a backslash becomes \\. So a
// sender cannot move the cursor, change the terminal or reorder a line, and
// a typed \x1b looks different from an escaped one. The left-to-right and
// right-to-left marks stay: they cannot reorder a line.
func Escape(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
