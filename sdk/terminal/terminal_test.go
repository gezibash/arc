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
		{"a C1 control introducer", "a\u009b31m", `a\x9b31m`},
		{"a typed escape differs from a real one", `\x1b`, `\\x1b`},
		{"a right-to-left override", "file\u202egnp.exe", `file\u202egnp.exe`},
		{"each embedding and override", "\u202a\u202b\u202c\u202d", `\u202a\u202b\u202c\u202d`},
		{"each isolate", "\u2066\u2067\u2068\u2069", `\u2066\u2067\u2068\u2069`},
		{"left-to-right and right-to-left marks stay", "\u05e9\u05dc\u05d5\u05dd\u200e!\u200f", "\u05e9\u05dc\u05d5\u05dd\u200e!\u200f"},
		{"line and paragraph separators", "a\u2028b\u2029c", `a\u2028b\u2029c`},
		{"a byte that is not UTF-8", "a\xffb\xc3", `a\xffb\xc3`},
		{"a real U+FFFD stays", "\ufffd", "\ufffd"},
		{"invisible characters", "\u200b\ufeff\u00ad\u2060\u2064\u206a\u206f", `\u200b\ufeff\u00ad\u2060\u2064\u206a\u206f`},
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
	for _, text := range []string{"", " \t\n", "\u200b", "\u2060\ufeff", "\U000e0067\U000e007f", "\u2028", "\x07 "} {
		if !Blank(text) {
			t.Errorf("Blank(%q) = false, want true: no character of it can be seen", text)
		}
	}
	for _, text := range []string{"a", " .", "\u200bx", "\U0001f989"} {
		if Blank(text) {
			t.Errorf("Blank(%q) = true, want false", text)
		}
	}
}
