package manifest

import (
	"strings"
	"testing"
)

// valid is a small data manifest that Parse takes. Each case of
// TestParseRefuses breaks one part of it.
const valid = `{"interface":1,"id":"notes","shape":"data",
"kinds":{"note":{"kind":30023,"visibility":"sealed"}},
"formats":{"line":{"record":"{{title}}"}},
"commands":[
 {"path":["add"],"args":[{"name":"text","kind":"positional","type":"text"}],
  "action":{"publish":{"kind":"note","d":"{{text}}","content":"{{text}}"}},"output":{}},
 {"path":["ls"],"action":{"query":{"kinds":["note"]}},"output":{}}]}`

func TestParseTakesAValidManifest(t *testing.T) {
	m, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "notes" || len(m.Commands) != 2 {
		t.Errorf("parsed %s with %d commands", m.ID, len(m.Commands))
	}
}

// TestParseRefuses breaks the manifest in each step of the check, and in
// the JSON, and wants the reason of the refusal.
func TestParseRefuses(t *testing.T) {
	cases := []struct {
		name, from, to, want string
	}{
		{"an id with capitals", `"id":"notes"`, `"id":"Notes"`, "must be lower-case letters"},
		{"a service with no section", `"shape":"data"`, `"shape":"service"`, "a service needs a service section"},
		{"a kind that is not an event kind", `"kind":30023`, `"kind":70000`, "is not an event kind"},
		{"a group kind with no group", `"visibility":"sealed"`, `"visibility":"group"`, "a group kind needs a group section"},
		{"a format that does not compile", `"record":"{{title}}"`, `"record":"{{title"`, "format line"},
		{"two commands with one path", `"path":["ls"]`, `"path":["add"]`, `two commands have the path "add"`},
		{"an argument of no type", `"type":"text"`, `"type":"words"`, `the type "words" does not exist`},
		{"a template that names no argument", `"content":"{{text}}"`, `"content":"{{other}}"`, `names "other", which is not an argument`},
		{"a sealed publish with no d", `"d":"{{text}}",`, ``, "a publish of a sealed kind needs a d tag"},
		{"a query of no kind", `"query":{"kinds":["note"]}`, `"query":{"kinds":[]}`, "a query names at least one kind"},
		{"a command with two actions", `"query":{"kinds":["note"]}`, `"query":{"kinds":["note"]},"delete":{"kind":"note","d":"x"}`, "exactly one action, not 2"},
		{"a time that only a notebook sets", `"content":"{{text}}"`, `"content":"{{text}}","at":5`, `unknown field "at"`},
	}
	for _, c := range cases {
		if !strings.Contains(valid, c.from) {
			t.Fatalf("%s: the manifest has no %s", c.name, c.from)
		}
		_, err := Parse([]byte(strings.Replace(valid, c.from, c.to, 1)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", c.name, err, c.want)
		}
	}
}
