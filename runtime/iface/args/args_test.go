package args

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/gezibash/arc/runtime/iface/manifest"
	"github.com/gezibash/arc/runtime/iface/template"
)

// resolver knows hex keys and npubs, and no names.
type resolver struct{}

func (resolver) ResolveKey(_ context.Context, text string) (nostr.PubKey, error) {
	if pk, err := nostr.PubKeyFromHex(text); err == nil {
		return pk, nil
	}
	if prefix, value, err := nip19.Decode(text); err == nil && prefix == "npub" {
		return value.(nostr.PubKey), nil
	}
	return nostr.PubKey{}, errors.New("unknown key " + text)
}

// command has a positional, an option, a switch and a variadic, in the
// order that a command line gives them.
var command = &manifest.Command{Args: []manifest.Arg{
	{Name: "to", Kind: "positional", Type: "text", Required: true},
	{Name: "count", Kind: "option", Type: "integer", Default: "3"},
	{Name: "quiet", Kind: "switch"},
	{Name: "argv", Kind: "positional", Type: "text", Variadic: true},
}}

func TestBindReadsACommandLine(t *testing.T) {
	cases := []struct {
		name  string
		words []string
		want  Values
		flags Flags
	}{
		{"a default fills an absent option", []string{"a"},
			Values{"to": "a", "count": int64(3)}, Flags{}},
		{"an option takes the next word", []string{"--count", "7", "a"},
			Values{"to": "a", "count": int64(7)}, Flags{}},
		{"an option takes its value after =", []string{"a", "--count=7"},
			Values{"to": "a", "count": int64(7)}, Flags{}},
		{"a switch is true", []string{"-quiet", "a"},
			Values{"to": "a", "count": int64(3), "quiet": true}, Flags{}},
		{"the flags of every command", []string{"--json", "--later", "--dry-run", "a"},
			Values{"to": "a", "count": int64(3)}, Flags{JSON: true, Later: true, DryRun: true}},
		{"a variadic keeps the flags after its first word", []string{"a", "sh", "-c", "--quiet"},
			Values{"to": "a", "count": int64(3), "argv": []string{"sh", "-c", "--quiet"}}, Flags{}},
		{"-- ends the flags", []string{"--", "--quiet", "x"},
			Values{"to": "--quiet", "count": int64(3), "argv": []string{"x"}}, Flags{}},
		{"a word of dashes is text", []string{"---", "-"},
			Values{"to": "---", "count": int64(3), "argv": []string{"-"}}, Flags{}},
	}
	for _, c := range cases {
		values, flags, err := Bind(context.Background(), command, c.words, resolver{}, nil)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(values, c.want) || flags != c.flags {
			t.Errorf("%s: got %v %+v, want %v %+v", c.name, values, flags, c.want, c.flags)
		}
	}
}

func TestBindRefuses(t *testing.T) {
	short := &manifest.Command{Args: []manifest.Arg{
		{Name: "to", Kind: "positional", Type: "text"},
		{Name: "count", Kind: "option", Type: "integer"},
		{Name: "quiet", Kind: "switch"},
	}}
	cases := []struct {
		name  string
		c     *manifest.Command
		words []string
		want  string
	}{
		{"a flag that no argument has", command, []string{"--loud", "a"}, "unknown flag --loud"},
		{"a positional named as a flag", command, []string{"--to", "a"}, "unknown flag --to"},
		{"a switch with a value", command, []string{"--quiet=yes", "a"}, "--quiet takes no value"},
		{"an option at the end", command, []string{"a", "--count"}, "--count needs a value"},
		{"a value of the wrong type", command, []string{"a", "--count", "many"}, `count: "many" is not a whole number`},
		{"an absent required argument", command, []string{"--quiet"}, "missing <to>"},
		{"a word with no argument", short, []string{"a", "b"}, `too many arguments: b, after the to is "a"`},
	}
	for _, c := range cases {
		_, _, err := Bind(context.Background(), c.c, c.words, resolver{}, nil)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

// TestHelpNeedsNoArguments asks for help with a required argument absent.
// The flags are read in order, so a bad flag before -h still fails.
func TestHelpNeedsNoArguments(t *testing.T) {
	if _, _, err := Bind(context.Background(), command, []string{"--loud", "-h"}, resolver{}, nil); err == nil {
		t.Error("an unknown flag before -h passed")
	}
	values, flags, err := Bind(context.Background(), command, []string{"-h"}, resolver{}, nil)
	if err != nil || !flags.Help || values != nil {
		t.Errorf("-h with no arguments: %v %+v %v", values, flags, err)
	}
}

func TestBindReadsTheStandardInput(t *testing.T) {
	c := &manifest.Command{Args: []manifest.Arg{
		{Name: "body", Kind: "positional", Type: "stdin"},
		{Name: "to", Kind: "positional", Type: "text"},
	}}
	values, _, err := Bind(context.Background(), c, []string{"a"}, resolver{}, strings.NewReader("hello"))
	if err != nil || values["body"] != "hello" || values["to"] != "a" {
		t.Errorf("got %v %v", values, err)
	}
	big := strings.NewReader(strings.Repeat("x", MaxInput+1))
	if _, _, err := Bind(context.Background(), c, nil, resolver{}, big); err == nil || !strings.Contains(err.Error(), "the standard input is over") {
		t.Errorf("an input over the limit: %v", err)
	}
}

func TestArgumentTypes(t *testing.T) {
	pk := nostr.Generate().Public()
	id := nostr.ID(pk)
	cases := []struct {
		arg  manifest.Arg
		in   string
		want any
		err  string
	}{
		{manifest.Arg{Type: "key"}, nip19.EncodeNpub(pk), pk.Hex(), ""},
		{manifest.Arg{Type: "key"}, "nobody", nil, "unknown key"},
		{manifest.Arg{Type: "event"}, nip19.EncodeNevent(id, nil, nostr.ZeroPK), id.Hex(), ""},
		{manifest.Arg{Type: "event"}, nip19.EncodeNaddr(pk, 30023, "page", nil), "30023:" + pk.Hex() + ":page", ""},
		{manifest.Arg{Type: "event"}, nip19.EncodeNpub(pk), nil, "not an event"},
		{manifest.Arg{Type: "integer"}, "12", int64(12), ""},
		{manifest.Arg{Type: "integer"}, "twelve", nil, "whole number"},
		{manifest.Arg{Type: "lines"}, "3:9", "3:9", ""},
		{manifest.Arg{Type: "lines"}, "9:3", nil, "ends before"},
		{manifest.Arg{Type: "lines"}, ":", nil, "not a range"},
		{manifest.Arg{Type: "address", Pattern: "^[a-z]+/[a-z]+$"}, "a/b", "a/b", ""},
		{manifest.Arg{Type: "address", Pattern: "^[a-z]+/[a-z]+$"}, "a/b/c", nil, "is not an address"},
		{manifest.Arg{Type: "path"}, "args_test.go", nil, "never replaces"},
	}
	for _, c := range cases {
		got, err := convert(context.Background(), c.arg, c.in, resolver{})
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s %q: got %v, want an error with %q", c.arg.Type, c.in, err, c.err)
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%s %q: got %v %v, want %v", c.arg.Type, c.in, got, err, c.want)
		}
	}
}

func TestFileArgumentsOfferTheirFields(t *testing.T) {
	path := t.TempDir() + "/note.txt"
	if err := os.WriteFile(path, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := convert(context.Background(), manifest.Arg{Type: "file"}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := template.Scope(func(string) (any, bool) { return value, true })
	tpl, _ := template.Compile("{{f.name}} {{f.type}} {{f.size}} {{f.sha256|short}} {{f}}", nil)
	out, err := tpl.Render(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "note.txt text/plain") || !strings.Contains(out, " 2 8f434346…") || !strings.HasSuffix(out, " aGk=") {
		t.Errorf("the file renders as %q", out)
	}
}
