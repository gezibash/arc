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
//   - a control character of U+0000 to U+001F or U+007F, other than a
//     newline or a tab, and each byte that is not UTF-8, becomes \xHH;
//   - a control character of U+0080 to U+009F, an embedding, override or
//     isolate of the bidirectional algorithm, a line or paragraph
//     separator, and an invisible character becomes \uHHHH;
//   - a tag character becomes \UHHHHHHHH, except in the flags of England,
//     Scotland and Wales;
//   - a backslash becomes \\.
//
// So a sender cannot move the cursor, change the terminal, reorder a line,
// break a line without a newline, or hide text that an agent reads and a
// person does not see. A typed \x1b looks different from an escaped one,
// and the byte 0x85 looks different from U+0085. The joiners U+200C and
// U+200D, and the marks U+200E, U+200F and U+061C, stay: emoji and
// right-to-left text use them, and they cannot hide text.
func Escape(text string) string { return escape(text, false, -1) }

// EscapeLine is Escape for text that must stay on one line, such as a
// title or a reason: it also writes a newline as \n and a tab as \t.
func EscapeLine(text string) string { return escape(text, true, -1) }

// EscapeLineCut is EscapeLine, cut to at most limit characters of output
// and then an ellipsis. It never cuts an escape or a flag.
func EscapeLineCut(text string, limit int) string { return escape(text, true, limit) }

// Strip removes each character that Escape escapes, except a backslash,
// and keeps the rest as it is. It is for the output of an app, which a
// person can read on a terminal and a program can read through a pipe:
// a program gets the backslashes of the value, and no escapes.
func Strip(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		piece, size := next(text[i:], false)
		if original := text[i : i+size]; piece == original || original == `\` {
			b.WriteString(original)
		}
		i += size
	}
	return b.String()
}

// escape writes text as Escape and EscapeLine say. With a limit of 0 or
// more, it stops before the piece that would make the output longer than
// limit characters, and writes an ellipsis.
func escape(text string, oneLine bool, limit int) string {
	var b strings.Builder
	b.Grow(len(text))
	written := 0
	for i := 0; i < len(text); {
		piece, size := next(text[i:], oneLine)
		if limit >= 0 {
			written += utf8.RuneCountInString(piece)
			if written > limit {
				b.WriteString("\u2026")
				break
			}
		}
		b.WriteString(piece)
		i += size
	}
	return b.String()
}

// next returns the escaped form of the character, or flag, at the start of
// text, and the number of bytes of text that it takes.
func next(text string, oneLine bool) (string, int) {
	r, size := utf8.DecodeRuneInString(text)
	if r == '\U0001f3f4' {
		if flag, ok := flagAt(text); ok {
			return flag, len(flag)
		}
	}
	switch {
	case r == utf8.RuneError && size == 1:
		return fmt.Sprintf(`\x%02x`, text[0]), 1
	case r == '\\':
		return `\\`, size
	case r == '\n' && oneLine:
		return `\n`, size
	case r == '\t' && oneLine:
		return `\t`, size
	case r == '\n' || r == '\t':
		return string(r), size
	case r < 0x80 && unicode.IsControl(r):
		return fmt.Sprintf(`\x%02x`, r), size
	case unicode.IsControl(r), hidden(r):
		return fmt.Sprintf(`\u%04x`, r), size
	case r >= '\U000e0000' && r <= '\U000e007f':
		return fmt.Sprintf(`\U%08x`, r), size
	}
	return text[:size], size
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
	case r >= '\ufff9' && r <= '\ufffb', r == '\u180e':
		return true // interlinear annotation, and the Mongolian vowel separator
	case r == '\u115f', r == '\u1160', r == '\u3164', r == '\uffa0':
		return true // Hangul fillers
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
// control characters, format characters (Unicode class Cf, such as U+200B
// or the tag characters), combining marks (classes Mn and Me) with no
// character to mark, the Hangul fillers and the blank Braille pattern.
func Blank(text string) bool {
	for _, r := range text {
		switch {
		case unicode.IsSpace(r), unicode.IsControl(r), unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me):
		case r == '\u115f', r == '\u1160', r == '\u3164', r == '\uffa0', r == '\u2800':
		default:
			return false
		}
	}
	return true
}
