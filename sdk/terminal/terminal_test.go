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
		{"emoji and accents stay", "\u00ebmoji \U0001f989", "\u00ebmoji \U0001f989"},
	} {
		if got := Escape(c.in); got != c.want {
			t.Errorf("%s: Escape(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
