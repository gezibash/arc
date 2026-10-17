package terminal

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Each expected value applies the rules of docs/clients/SPEC.md, section
// 3.9, by hand.
func TestEscape(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"plain text stays", "hello, Zim", "hello, Zim"},
		{"newline and tab stay", "one\ttwo\nthree", "one\ttwo\nthree"},
		{"an ANSI sequence", "\x1b[2Jgone", `\x1b[2Jgone`},
		{"a carriage return and a bell", "a\rb\x07", `a\x0db\x07`},
		{"DEL", "a\x7f", `a\x7f`},
		{"a C1 control introducer", "a\u009b31m", `a\u009b31m`},
		{"the byte 0x85 and U+0085 look different", "\x85\u0085", `\x85\u0085`},
		{"a typed escape differs from a real one", `\x1b`, `\\x1b`},
		{"a right-to-left override", "file\u202egnp.exe", `file\u202egnp.exe`},
		{"each embedding and override", "\u202a\u202b\u202c\u202d", `\u202a\u202b\u202c\u202d`},
		{"each isolate", "\u2066\u2067\u2068\u2069", `\u2066\u2067\u2068\u2069`},
		{"left-to-right and right-to-left marks stay", "\u05e9\u05dc\u05d5\u05dd\u200e!\u200f", "\u05e9\u05dc\u05d5\u05dd\u200e!\u200f"},
		{"line and paragraph separators", "a\u2028b\u2029c", `a\u2028b\u2029c`},
		{"a byte that is not UTF-8", "a\xffb\xc3", `a\xffb\xc3`},
		{"a real U+FFFD stays", "\ufffd", "\ufffd"},
		{"invisible characters", "\u200b\ufeff\u00ad\u2060\u2064\u206a\u206f", `\u200b\ufeff\u00ad\u2060\u2064\u206a\u206f`},
		{"annotation characters and the Mongolian vowel separator", "a\ufff9b\ufffac\ufffbd\u180e", `a\ufff9b\ufffac\ufffbd\u180e`},
		{"Hangul fillers", "\u115f\u1160\u3164\uffa0", `\u115f\u1160\u3164\uffa0`},
		{"the combining grapheme joiner", "a\u034fb", `a\u034fb`},
		{"a Mongolian free variation selector", "\u1820\u180b", "\u1820" + `\u180b`},
		{"the musical format characters", "a\U0001d173b\U0001d17a", `a\U0001d173b\U0001d17a`},
		{"tags that hide text", "hi\U000e0068\U000e0069", `hi\U000e0068\U000e0069`},
		{"the flag of England stays", "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f", "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"},
		{"the flag of Scotland stays", "\U0001f3f4\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f", "\U0001f3f4\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f"},
		{"the flag of Wales stays", "\U0001f3f4\U000e0067\U000e0062\U000e0077\U000e006c\U000e0073\U000e007f", "\U0001f3f4\U000e0067\U000e0062\U000e0077\U000e006c\U000e0073\U000e007f"},
		{"another tag sequence", "\U0001f3f4\U000e0075\U000e0073\U000e007f", "\U0001f3f4" + `\U000e0075\U000e0073\U000e007f`},
		{"a flag cut short", "\U0001f3f4\U000e0067\U000e0062", "\U0001f3f4" + `\U000e0067\U000e0062`},
		{"joiners and the Arabic letter mark stay", "\U0001f468\u200d\U0001f469 a\u200cb x\u061cy", "\U0001f468\u200d\U0001f469 a\u200cb x\u061cy"},
		{"one emoji selector after a visible character stays", "\u2764\ufe0f", "\u2764\ufe0f"},
		{"a keycap stays", "1\ufe0f\u20e3", "1\ufe0f\u20e3"},
		{"a selector alone", "\ufe0f", `\ufe0f`},
		{"a second selector in a row", "\u2764\ufe0f\ufe0f", "\u2764\ufe0f" + `\ufe0f`},
		{"a selector after a space", "a \ufe0f", "a " + `\ufe0f`},
		{"a selector after an escaped character", "\u200b\ufe0f", `\u200b\ufe0f`},
		{"an ideographic selector after a CJK ideograph stays", "\u845b\U000e0100", "\u845b\U000e0100"},
		{"an ideographic selector after U+3007, an ideograph", "\u3007\U000e0100", "\u3007\U000e0100"},
		{"an ideographic selector after U+3005, a Han mark that is not an ideograph", "\u3005\U000e0100", "\u3005" + `\U000e0100`},
		{"an emoji selector after a backslash", "\\\ufe0f", `\\` + "\ufe0f"},
		{"an ideographic selector after a letter", "a\U000e0100", "a" + `\U000e0100`},
		{"a second ideographic selector", "\u845b\U000e0100\U000e0101", "\u845b\U000e0100" + `\U000e0101`},
		{"emoji and accents stay", "\u00ebmoji \U0001f989", "\u00ebmoji \U0001f989"},
	} {
		if got := Escape(c.in); got != c.want {
			t.Errorf("%s: Escape(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestBlank(t *testing.T) {
	for _, text := range []string{"", " \t\n", "\u200b", "\u2060\ufeff", "\U000e0067\U000e007f", "\u2028", "\x07 ", "\u3164\u115f\u1160\uffa0", "\u2800", "\u0301\u20dd", "\ufe0f"} {
		if !Blank(text) {
			t.Errorf("Blank(%q) = false, want true: no character of it can be seen", text)
		}
	}
	for _, text := range []string{"a", " .", "\u200bx", "\U0001f989", "e\u0301", "\u3164x"} {
		if Blank(text) {
			t.Errorf("Blank(%q) = true, want false", text)
		}
	}
}

func TestEscapeLine(t *testing.T) {
	if got, want := EscapeLine("title\nfake header\tx\u202e"), `title\nfake header\tx\u202e`; got != want {
		t.Errorf("EscapeLine = %q, want %q", got, want)
	}
}

// The limit counts characters of output, and a cut never splits an escape
// or a flag.
func TestEscapeLineCut(t *testing.T) {
	england := "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"
	for _, c := range []struct {
		name, in string
		limit    int
		want     string
	}{
		{"short text stays whole", "ring failed", 20, "ring failed"},
		{"a text of exactly the limit", "abcde", 5, "abcde"},
		{"one character more", "abcdef", 5, "abcd\u2026"},
		{"an escape that does not fit is left out whole", "abc\x1b", 5, "abc\u2026"},
		{"an escape that fits", "ab\x1b", 6, `ab\x1b`},
		{"tags count as their escapes", "a\U000e0068", 5, "a\u2026"},
		{"a flag is never split", "ab" + england, 5, "ab\u2026"},
		{"a flag that fits", "a" + england, 8, "a" + england},
	} {
		if got := EscapeLineCut(c.in, c.limit); got != c.want {
			t.Errorf("%s: EscapeLineCut(%q, %d) = %q, want %q", c.name, c.in, c.limit, got, c.want)
		}
	}
}

func TestStrip(t *testing.T) {
	england := "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"
	in := "a\x1b[2Jb\r\nc\u202ed\u200be\U000e0068f\xff g\\h\u200e\u200d i" + england
	want := "a[2Jb\nc" + "def g\\h\u200e\u200d i" + england
	if got := Strip(in); got != want {
		t.Errorf("Strip(%q) = %q, want %q", in, got, want)
	}
}

func TestStripKeepsOneSelector(t *testing.T) {
	if got, want := Strip("\u2764\ufe0f\ufe0f a\U000e0100"), "\u2764\ufe0f a"; got != want {
		t.Errorf("Strip = %q, want %q", got, want)
	}
	if got := Stripped("\u2764\ufe0f\ufe0f a\U000e0100\x1b"); got != 3 {
		t.Errorf("Stripped = %d, want 3", got)
	}
}

// EscapeData escapes what Escape escapes, and keeps one backslash.
func TestEscapeData(t *testing.T) {
	if got, want := EscapeData("a\\b\x1b\u200bc"), `a\b\x1b\u200bc`; got != want {
		t.Errorf("EscapeData = %q, want %q", got, want)
	}
}

// EscapeJSON keeps a JSON text valid, with the same value, and writes the
// hidden characters inside its strings as JSON escapes.
func TestEscapeJSON(t *testing.T) {
	in := `{"a":"x` + "\u200b" + `y\\z","b":"` + "\u202e" + `q","c":[1,"\u0041"],"d":"` + "\u845b\U000e0100 \U000e0068" + `"}`
	want := `{"a":"x\u200by\\z","b":"\u202eq","c":[1,"\u0041"],"d":"` + "\u845b\U000e0100" + ` \udb40\udc68"}`
	got := EscapeJSON(in)
	if got != want {
		t.Errorf("EscapeJSON = %q, want %q", got, want)
	}
	var before, after any
	if err := json.Unmarshal([]byte(in), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(got), &after); err != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("EscapeJSON changed the value: %v, %v", after, err)
	}
}

// The cases of Alfred's probe of ac2ff05, with the values that the rules of
// docs/clients/SPEC.md, section 3.9, give.
func TestStripCases(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a\r\nb", "a\nb"},
		{"\ufeffBOM", "BOM"},
		{"caf\xe9", "caf"},
		{"x\x1b\x00[31my", "x[31my"},
		{"\\\x00x1b", `\x1b`},
		{"line1\nline2\tTAB", "line1\nline2\tTAB"},
		{"ig\u200bnore", "ignore"},
		{"\U0001f600\ufe00\U000e0100\U000e0150", "\U0001f600\ufe00"},
		{"pay \ufff9alice\ufffabob\ufffb", "pay alicebob"},
		{"\u202egnp.exe", "gnp.exe"},
		{"x\u061c\u200f(1)\u200fy", "x\u061c\u200f(1)\u200fy"},
		{"hi\u034fd\u180be\U0001d173\U0001d174\U0001d175\U0001d176\U0001d177\U0001d178\U0001d179\U0001d17an", "hiden"},
	} {
		if got := Strip(c.in); got != c.want {
			t.Errorf("Strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Strip keeps what Escape writes as itself: after a backslash, one
// selector stays in both.
func TestStripAndEscapeAgreeAfterABackslash(t *testing.T) {
	in := "\\\ufe0f"
	if got, want := Strip(in), in; got != want {
		t.Errorf("Strip(%q) = %q, want %q", in, got, want)
	}
	if got, want := Escape(in), `\\`+"\ufe0f"; got != want {
		t.Errorf("Escape(%q) = %q, want %q", in, got, want)
	}
}

// No cut is longer than its limit, the ellipsis included.
func TestEscapeLineCutNeverPassesTheLimit(t *testing.T) {
	text := "ab\x1bcd\U000e0068ef\u202egh"
	for limit := 1; limit <= 40; limit++ {
		if got := EscapeLineCut(text, limit); utf8.RuneCountInString(got) > limit {
			t.Errorf("EscapeLineCut(%q, %d) = %q: %d characters", text, limit, got, utf8.RuneCountInString(got))
		}
	}
}

// Width counts two columns for a wide character, none for a mark or a
// format character, and one for each other character.
func TestWidth(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{"\u6f22\u5b57", 4}, // two CJK ideographs
		{"\uff21", 2},       // a fullwidth A
		{"\U0001f600", 2},   // an emoji
		{"e\u0301", 1},      // a combining acute accent
		{"\u2764\ufe0f", 2}, // the emoji selector makes a heart wide
		{"\u2764", 1},       // a heart alone is narrow
		{"a\u200db", 2},     // a joiner between letters takes no column
		{"\U0001f468\u200d\U0001f469\u200d\U0001f467", 2}, // a family
		{"\U0001f44d\U0001f3fd", 2},                       // a thumb with a skin tone
		{"\U0001f1fa\U0001f1f8", 2},                       // a flag
		{"\U0001f1fa", 2},                                 // a lone regional indicator
		{"\u0915\u093f", 2},                               // Devanagari ka and the spacing vowel sign i
		{"a" + strings.Repeat("\u0301", 300), 1},          // marks take no column
		{"\u2026", 1},                                     // the ellipsis
		{`\u200b`, 6},                                     // an escape
	} {
		if got := Width(c.in); got != c.want {
			t.Errorf("Width(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// EscapeLineFit cuts by columns, and by characters: 100 CJK ideographs fit
// in 72 columns as 35 of them and an ellipsis, 100 hearts with the emoji
// selector as 35, and a letter with 300 marks, which takes one column, as
// nothing. It cuts between clusters: never inside an escape, a flag, or a
// letter and its marks.
func TestEscapeLineFit(t *testing.T) {
	for _, c := range []struct {
		name, in string
		columns  int
		want     string
	}{
		{"hearts", strings.Repeat("\u2764\ufe0f", 100), 72, strings.Repeat("\u2764\ufe0f", 35) + "\u2026"},
		{"a pile of marks", "a" + strings.Repeat("\u0301", 300), 72, "\u2026"},
		{"flags", strings.Repeat("\U0001f1fa\U0001f1f8", 50), 9, strings.Repeat("\U0001f1fa\U0001f1f8", 4) + "\u2026"},
		{"letters with marks", strings.Repeat("e\u0301", 100), 10, strings.Repeat("e\u0301", 9) + "\u2026"},
		{"marks that fit", "e\u0301e\u0301", 72, "e\u0301e\u0301"},
	} {
		if got := EscapeLineFit(c.in, c.columns); got != c.want {
			t.Errorf("%s: EscapeLineFit is %q, want %q", c.name, got, c.want)
		}
	}
	cjk := strings.Repeat("\u6f22", 100)
	if got, want := EscapeLineFit(cjk, 72), strings.Repeat("\u6f22", 35)+"\u2026"; got != want {
		t.Errorf("EscapeLineFit of 100 ideographs is %q, want %q", got, want)
	}
	if got, want := EscapeLineFit("ab\u200bcd", 5), "ab\u2026"; got != want {
		t.Errorf("EscapeLineFit cut an escape: %q, want %q", got, want)
	}
	if got := EscapeLineFit("short", 72); got != "short" {
		t.Errorf("EscapeLineFit changed a short line: %q", got)
	}
}
