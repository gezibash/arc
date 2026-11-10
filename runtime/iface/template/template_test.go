package template

import (
	"strings"
	"testing"
)

// helpers is a Helpers for tests: it marks what it touched.
type helpers struct{}

func (helpers) Keyed(purpose, input string) (string, error) { return purpose + "/" + input, nil }
func (helpers) EventAuthor(ref string) (string, error)      { return "author-of-" + ref, nil }
func (helpers) Name(key string) string                      { return "name-of-" + key }
func (helpers) Sanitize(text string) string                 { return strings.ReplaceAll(text, "\x1b", "") }

// fields is a value with fields of its own, as a file argument is.
type fields map[string]string

func (f fields) Field(name string) any { return f[name] }

func scope(values map[string]any) Scope {
	return func(name string) (any, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// TestCompileRefusesABadTemplate checks each error of Compile.
func TestCompileRefusesABadTemplate(t *testing.T) {
	known := func(name string) bool { return name == "a" }
	cases := map[string]string{
		"{{a":              "does not close it",
		"{{A}}":            "does not name a value",
		"{{a|nope}}":       `the filter "nope" does not exist`,
		"{{a|keyed}}":      `the filter "keyed" needs an argument`,
		"{{a|json:x}}":     `the filter "json" takes no argument`,
		"{{a|truncate:0}}": "truncate needs a positive number",
		"{{b}}":            `names "b", which is not an argument`,
		"{{a+b}}":          `names "b", which is not an argument`,
	}
	for text, want := range cases {
		_, err := Compile(text, known)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%q): got %v, want an error with %q", text, err, want)
		}
	}
	if _, err := Compile("{{a.b|hex}} and {{a}}", known); err != nil {
		t.Errorf("a dotted name of a known root: %v", err)
	}
}

// TestRender renders names, fields, list items and filters.
func TestRender(t *testing.T) {
	values := scope(map[string]any{
		"who":  map[string]any{"name": "ada"},
		"list": []any{"zero", "one"},
		"file": fields{"size": "2"},
		"text": "abcdef",
		"hi":   "hi",
		"two":  "a\nb",
	})
	cases := map[string]string{
		"Hi {{who.name}}":           "Hi ada",
		"{{list.1}}":                "one",
		"{{list.5}}|{{missing.x}}|": "||",
		"{{file.size}}":             "2",
		"{{nothing|default:none}}":  "none",
		"{{hi|hex}}":                "6869",
		"{{text|truncate:3}}":       "abc…",
		"{{two|indent}}":            "  a\n  b",
		"{{hi|keyed:p}}":            "p/hi",
		"{{hi|event_author}}":       "author-of-hi",
		"{{who.name}}{{who.name}}!": "adaada!",
	}
	for text, want := range cases {
		tpl, err := Compile(text, nil)
		if err != nil {
			t.Fatalf("Compile(%q): %v", text, err)
		}
		got, err := tpl.Render(values, helpers{}, false)
		if err != nil || got != want {
			t.Errorf("Render(%q): got %q, %v; want %q", text, got, err, want)
		}
	}
}

// TestRenderCleansOnlyWhatItShows cleans the text and the values of a
// template that is shown, and leaves a template that is data as it is.
func TestRenderCleansOnlyWhatItShows(t *testing.T) {
	tpl, err := Compile("\x1b[2J{{v}}", nil)
	if err != nil {
		t.Fatal(err)
	}
	values := scope(map[string]any{"v": "x\x1b[31m"})
	if got, _ := tpl.Render(values, helpers{}, true); got != "[2Jx[31m" {
		t.Errorf("shown: got %q", got)
	}
	if got, _ := tpl.Render(values, helpers{}, false); got != "\x1b[2Jx\x1b[31m" {
		t.Errorf("data: got %q", got)
	}
}

// TestPlaceholderOnly finds a template that is one placeholder and
// nothing else.
func TestPlaceholderOnly(t *testing.T) {
	cases := map[string]string{"{{path}}": "path", "{{path|hex}}": "", "x{{path}}": "", "x": "", "{{a}}{{b}}": ""}
	for text, want := range cases {
		tpl, err := Compile(text, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := tpl.PlaceholderOnly()
		if got != want || ok != (want != "") {
			t.Errorf("%q: got %q, %v; want %q", text, got, ok, want)
		}
	}
}
