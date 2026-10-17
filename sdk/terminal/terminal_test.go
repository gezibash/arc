package terminal

import "testing"

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
		{"tags that hide text", "hi\U000e0068\U000e0069", `hi\U000e0068\U000e0069`},
		{"the flag of England stays", "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f", "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"},
		{"the flag of Scotland stays", "\U0001f3f4\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f", "\U0001f3f4\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f"},
		{"the flag of Wales stays", "\U0001f3f4\U000e0067\U000e0062\U000e0077\U000e006c\U000e0073\U000e007f", "\U0001f3f4\U000e0067\U000e0062\U000e0077\U000e006c\U000e0073\U000e007f"},
		{"another tag sequence", "\U0001f3f4\U000e0075\U000e0073\U000e007f", "\U0001f3f4" + `\U000e0075\U000e0073\U000e007f`},
		{"a flag cut short", "\U0001f3f4\U000e0067\U000e0062", "\U0001f3f4" + `\U000e0067\U000e0062`},
		{"joiners and the Arabic letter mark stay", "\U0001f468\u200d\U0001f469 a\u200cb x\u061cy", "\U0001f468\u200d\U0001f469 a\u200cb x\u061cy"},
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
		{"one character more", "abcdef", 5, "abcde\u2026"},
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
