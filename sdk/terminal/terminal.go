// Package terminal makes text from another citizen safe to show on a
// terminal, as docs/clients/SPEC.md, section 3.9, defines.
package terminal

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// flags are the three subdivision flags that the tag characters may spell:
// England, Scotland and Wales. Each is U+1F3F4, the tags of its code, and
// the cancel tag U+E007F.
var flags = []string{
	"\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f",
	"\U0001f3f4\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f",
	"\U0001f3f4\U000e0067\U000e0062\U000e0077\U000e006c\U000e0073\U000e007f",
}

// Escape writes each character of text that can change a terminal, or hide
// text from the person who reads it, as an escape that a person can read:
//
//   - a control character other than a newline or a tab, and each byte
//     that is not UTF-8, becomes \xHH;
//   - an embedding, override or isolate of the bidirectional algorithm, a
//     line or paragraph separator, and an invisible character becomes
//     \uHHHH;
//   - a tag character becomes \UHHHHHHHH, except in the flags of England,
//     Scotland and Wales;
//   - a backslash becomes \\.
//
// So a sender cannot move the cursor, change the terminal, reorder a line,
// break a line without a newline, or hide text that an agent reads and a
// person does not see. A typed \x1b looks different from an escaped one.
// The joiners U+200C and U+200D, and the marks U+200E, U+200F and U+061C,
// stay: emoji and right-to-left text use them, and they cannot hide text.
func Escape(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '\U0001f3f4' {
			if flag, ok := flagAt(text[i:]); ok {
				b.WriteString(flag)
				i += len(flag)
				continue
			}
		}
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, text[i])
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case hidden(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		case r >= '\U000e0000' && r <= '\U000e007f':
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// hidden reports a character of the Basic Multilingual Plane that Escape
// writes as \uHHHH.
func hidden(r rune) bool {
	switch {
	case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
		return true // embeddings, overrides and isolates
	case r == '\u2028', r == '\u2029':
		return true // line and paragraph separators
	case r == '\u200b', r == '\ufeff', r == '\u00ad',
		r >= '\u2060' && r <= '\u2064', r >= '\u206a' && r <= '\u206f':
		return true // invisible characters
	}
	return false
}

// flagAt returns the flag at the start of text, if text starts with one.
func flagAt(text string) (string, bool) {
	for _, flag := range flags {
		if strings.HasPrefix(text, flag) {
			return flag, true
		}
	}
	return "", false
}

// Blank reports a text that a person cannot see: it holds only spaces,
// control characters and format characters (Unicode class Cf), such as
// U+200B or the tag characters.
func Blank(text string) bool {
	for _, r := range text {
		if !unicode.IsSpace(r) && !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
