// Package terminal makes text from another citizen safe to show on a
// terminal, as docs/clients/SPEC.md, section 3.9, defines.
package terminal

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/width"
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
//     separator, an invisible character, and a variation selector that
//     selects nothing becomes \uHHHH;
//   - a tag character becomes \UHHHHHHHH, except in the flags of England,
//     Scotland and Wales, and so does a variation selector of U+E0100 to
//     U+E01EF that selects nothing, and a musical format character of
//     U+1D173 to U+1D17A;
//   - a backslash becomes \\.
//
// So a sender cannot move the cursor, change the terminal, reorder a line,
// break a line without a newline, or hide text that an agent reads and a
// person does not see. A typed \x1b looks different from an escaped one,
// and the byte 0x85 looks different from U+0085. The joiners U+200C and
// U+200D, and the marks U+200E, U+200F and U+061C, stay: emoji and
// right-to-left text use them, and they cannot hide text. One variation
// selector of U+FE00 to U+FE0F after a visible character stays, and one of
// U+E0100 to U+E01EF after a CJK ideograph: emoji and names use them.
func Escape(text string) string { return mode{limit: -1}.apply(text) }

// EscapeLine is Escape for text that must stay on one line, such as a
// title or a reason: it also writes a newline as \n and a tab as \t.
func EscapeLine(text string) string { return mode{oneLine: true, limit: -1}.apply(text) }

// EscapeLineCut is EscapeLine, cut to at most limit characters of output
// and then an ellipsis. It never cuts an escape or a flag.
func EscapeLineCut(text string, limit int) string {
	return mode{oneLine: true, limit: limit}.apply(text)
}

// EscapeLineFit is EscapeLine for a line that must fit in a number of
// columns of a terminal, as Width counts them. It also holds at most
// MaxRunesPerColumn characters for each column, so a pile of marks that
// takes no column cannot make the line long. A line that does not fit ends
// with an ellipsis after the clusters that fit. It cuts only between
// clusters: never inside an escape, a flag, an emoji joined of several, or
// between a character and its marks.
func EscapeLineFit(text string, columns int) string {
	if columns < 1 {
		return ""
	}
	all := clusters(escapedPieces(text))
	total, runes := 0, 0
	for _, c := range all {
		total += c.width()
		runes += utf8.RuneCountInString(c.text)
	}
	var b strings.Builder
	if total <= columns && runes <= MaxRunesPerColumn*columns {
		for _, c := range all {
			b.WriteString(c.text)
		}
		return b.String()
	}
	// The ellipsis takes one column and one character.
	total, runes = 0, 0
	for _, c := range all {
		w, n := c.width(), utf8.RuneCountInString(c.text)
		if total+w > columns-1 || runes+n > MaxRunesPerColumn*columns-1 {
			break
		}
		total, runes = total+w, runes+n
		b.WriteString(c.text)
	}
	return b.String() + "\u2026"
}

// MaxRunesPerColumn is the most characters for each column of a line that
// EscapeLineFit makes. Scripts with marks, such as Devanagari, hold fewer
// than two; a pile of marks that hides a line holds many more.
const MaxRunesPerColumn = 3

// Width is the number of columns that a terminal gives text, one cluster
// at a time. A cluster is a character with its marks, its variation
// selectors and its skin tone, or characters joined by U+200D, or a pair
// of regional indicators (a flag). A cluster takes two columns when it is
// a flag or a lone regional indicator, when it holds the emoji selector
// U+FE0F, or when its first character is wide or fullwidth by East Asian
// Width, such as a CJK ideograph or most emoji. A cluster whose first
// character is a mark or a format character takes none, and each other
// cluster one. Each spacing mark (class Mc) adds one. Width can count more
// columns than a terminal shows, and not fewer.
func Width(text string) int {
	var pieces []piece
	for i, r := range text {
		pieces = append(pieces, piece{text: text[i : i+utf8.RuneLen(r)], r: r, shown: true})
	}
	n := 0
	for _, c := range clusters(pieces) {
		n += c.width()
	}
	return n
}

// piece is the output of one character, or flag, of a text: the
// character itself when shown, else its escape.
type piece struct {
	text  string
	r     rune
	shown bool
}

// escapedPieces is text as EscapeLine writes it, one piece at a time.
func escapedPieces(text string) []piece {
	var out []piece
	prev := rune(-1)
	for i := 0; i < len(text); {
		p, size := next(text[i:], true, prev)
		original := text[i : i+size]
		r, _ := utf8.DecodeRuneInString(original)
		shown := p == original
		out = append(out, piece{text: p, r: r, shown: shown})
		prev = -1
		if shown || original == `\` {
			prev, _ = utf8.DecodeLastRuneInString(original)
		}
		i += size
	}
	return out
}

// cluster is the output of one or more pieces that a cut keeps together.
type cluster struct {
	text    string
	escaped bool
}

// clusters joins pieces into clusters. An escape is a cluster of its own,
// and nothing joins it. U+200D joins two pictographs only, so that letters
// around a joiner keep a column each.
func clusters(pieces []piece) []cluster {
	var out []cluster
	last := rune(-1)  // the last character of the open cluster, if shown
	first := rune(-1) // the first character of the open cluster
	flags := 0        // the regional indicators of the open cluster
	for _, p := range pieces {
		joins := p.shown && last >= 0 && (extends(p.r) ||
			(last == '\u200d' && pictograph(first) && pictograph(p.r)) ||
			(regional(p.r) && flags == 1))
		if joins {
			out[len(out)-1].text += p.text
		} else {
			out = append(out, cluster{text: p.text, escaped: !p.shown})
			first, flags = p.r, 0
		}
		if regional(p.r) {
			flags++
		}
		last = -1
		if p.shown {
			last, _ = utf8.DecodeLastRuneInString(p.text)
		}
	}
	return out
}

// extends reports a character that belongs to the cluster before it: a
// mark, the joiner U+200D, a variation selector, or a skin tone.
func extends(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc) || r == '\u200d' ||
		(r >= '\ufe00' && r <= '\ufe0f') || (r >= '\U000e0100' && r <= '\U000e01ef') ||
		(r >= '\U0001f3fb' && r <= '\U0001f3ff')
}

func regional(r rune) bool { return r >= '\U0001f1e6' && r <= '\U0001f1ff' }

// pictograph reports a character of the blocks of emoji and symbols that
// U+200D joins into one emoji.
func pictograph(r rune) bool {
	return (r >= '\U0001f000' && r <= '\U0001faff') || (r >= '\u2600' && r <= '\u27bf') || (r >= '\u2b00' && r <= '\u2bff')
}

func (c cluster) width() int {
	if c.escaped {
		return len(c.text) // an escape is ASCII
	}
	first, _ := utf8.DecodeRuneInString(c.text)
	switch {
	case strings.ContainsRune(c.text, '\ufe0f'), regional(first):
		// A lone regional indicator takes two columns on some terminals.
		return 2
	}
	n := runeWidth(first)
	for _, r := range c.text {
		if unicode.Is(unicode.Mc, r) {
			n++ // a spacing mark takes a column of its own
		}
	}
	return n
}

func runeWidth(r rune) int {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// EscapeData is Escape for text that a program reads through a pipe, such
// as the reply of a call: it escapes the same characters, and keeps a
// backslash as one backslash, so the rest of the text stays as it was.
func EscapeData(text string) string { return mode{backslash: true, limit: -1}.apply(text) }

// Strip removes each character that Escape escapes, except a backslash,
// and keeps the rest as it is. It is for the output of an app, which a
// person can read on a terminal and a program can read through a pipe:
// a program gets the backslashes of the value, and no escapes.
func Strip(text string) string { return mode{strip: true, backslash: true, limit: -1}.apply(text) }

// Stripped reports how many characters Strip removes from text.
func Stripped(text string) int {
	return utf8.RuneCountInString(text) - utf8.RuneCountInString(Strip(text))
}

// EscapeJSON is EscapeData for a JSON text: inside each string, it writes
// each character that Escape escapes as a JSON escape, \uHHHH, or a pair
// of them for a character outside the Basic Multilingual Plane. The text
// stays valid JSON with the same value, and every other byte stays as it
// was. text must be valid JSON.
func EscapeJSON(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	inString := false
	prev := rune(-1)
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case !inString:
			inString = c == '"'
			prev = -1
			b.WriteByte(c)
			i++
			continue
		case c == '"':
			inString = false
			b.WriteByte(c)
			i++
			continue
		case c == '\\':
			// An escape of JSON stays whole: \uHHHH, or a backslash and one
			// character.
			n := 2
			if i+1 < len(text) && text[i+1] == 'u' {
				n = 6
			}
			n = min(n, len(text)-i)
			b.WriteString(text[i : i+n])
			prev = -1
			i += n
			continue
		}
		piece, size := next(text[i:], false, prev)
		original := text[i : i+size]
		if piece == original {
			b.WriteString(original)
			prev, _ = utf8.DecodeLastRuneInString(original)
		} else {
			for _, r := range original {
				if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
					fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			}
			prev = -1
		}
		i += size
	}
	return b.String()
}

// mode says how apply writes a text.
type mode struct {
	oneLine   bool // a newline and a tab become \n and \t
	backslash bool // a backslash stays one backslash
	strip     bool // a character that would be escaped is removed
	limit     int  // the most characters of output, or -1 for no limit
}

// apply writes text as the mode says. With a limit of 0 or more, an output
// longer than limit characters ends with an ellipsis after the pieces that
// fit in limit - 1 characters, so the output is never longer than limit.
func (m mode) apply(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	// written counts the characters of output; keep is the length in bytes
	// of the output that fits in limit - 1 characters.
	written, keep := 0, 0
	// prev is the last character written as itself, or -1: a variation
	// selector stays only after a character that it can select.
	prev := rune(-1)
	for i := 0; i < len(text); {
		piece, size := next(text[i:], m.oneLine, prev)
		original := text[i : i+size]
		shown := piece == original
		switch {
		case original == `\` && m.backslash:
			piece, shown = original, true
		case m.strip && !shown:
			piece = ""
		}
		b.WriteString(piece)
		if m.limit >= 0 {
			written += utf8.RuneCountInString(piece)
			if written < m.limit {
				keep = b.Len()
			}
			if written > m.limit {
				return b.String()[:keep] + "\u2026"
			}
		}
		// A backslash is a visible character, written as one backslash or
		// as two, so Escape and Strip treat what follows it alike.
		prev = -1
		if shown || original == `\` {
			prev, _ = utf8.DecodeLastRuneInString(original)
		}
		i += size
	}
	return b.String()
}

// next returns the escaped form of the character, or flag, at the start of
// text, and the number of bytes of text that it takes. prev is the
// character before it, if that was written as itself, or -1.
func next(text string, oneLine bool, prev rune) (string, int) {
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
	case r >= '\ufe00' && r <= '\ufe0f':
		if selectable(prev) {
			return text[:size], size
		}
		return fmt.Sprintf(`\u%04x`, r), size
	case r >= '\U000e0100' && r <= '\U000e01ef':
		if prev >= 0 && unicode.Is(unicode.Ideographic, prev) {
			return text[:size], size
		}
		return fmt.Sprintf(`\U%08x`, r), size
	case unicode.IsControl(r), hidden(r):
		return fmt.Sprintf(`\u%04x`, r), size
	case r >= '\U0001d173' && r <= '\U0001d17a':
		return fmt.Sprintf(`\U%08x`, r), size // musical format characters
	case r >= '\U000e0000' && r <= '\U000e007f':
		return fmt.Sprintf(`\U%08x`, r), size
	}
	return text[:size], size
}

// selectable reports a character that a variation selector of U+FE00 to
// U+FE0F can follow: a visible character that is not a mark. A variation
// selector is a mark, so a second one in a row selects nothing.
func selectable(r rune) bool {
	return r >= 0 && unicode.IsGraphic(r) && !unicode.IsSpace(r) && !unicode.In(r, unicode.Mn, unicode.Me)
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
	case r >= '\ufff9' && r <= '\ufffb', r == '\u180e', r == '\u180b':
		return true // interlinear annotation, the Mongolian vowel separator, and free variation selector one
	case r == '\u034f':
		return true // the combining grapheme joiner
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
