package iface

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// specManifests are the manifests of section 17 of the spec.
func specManifests(t *testing.T) map[string]*Manifest {
	t.Helper()
	body, err := os.ReadFile("../docs/interface/SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	text = text[strings.Index(text, "\n## 17. "):]
	blocks := regexp.MustCompile("(?s)```json\n(\\{\n  \"interface\".*?)```").FindAllStringSubmatch(text, -1)
	out := map[string]*Manifest{}
	for _, block := range blocks {
		m, err := Parse([]byte(block[1]))
		if err != nil {
			t.Fatalf("a manifest of the spec does not parse: %v\n%s", err, block[1][:80])
		}
		out[m.ID] = m
	}
	return out
}

func TestEveryManifestOfTheSpecParses(t *testing.T) {
	got := specManifests(t)
	for _, id := range []string{"exec", "sqlite", "releases", "journal", "dm", "agora", "files"} {
		if got[id] == nil {
			t.Errorf("the spec has no manifest %s that parses", id)
		}
	}
}

const minimal = `{"interface": 1, "id": "echo", "shape": "service", "title": "Echo",
 "service": {"method": "EXEC", "path": "/"}, "kinds": {}, "formats": {},
 "commands": [{"path": [], "summary": "s", "action": {"call": {"class": "live", "body": "x"}}}]}`

func TestParseRefusesWhatVersionOneDoesNotDefine(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"a later version":   {`"interface": 1`, `"interface": 2`, "needs interface version 2"},
		"no version":        {`"interface": 1,`, ``, "names no interface version"},
		"an unknown field":  {`"title": "Echo"`, `"title": "Echo", "color": "red"`, "unknown field"},
		"a bad id":          {`"id": "echo"`, `"id": "Echo!"`, "the id"},
		"a call in data":    {`"shape": "service"`, `"shape": "data"`, "no service section"},
		"two actions":       {`"action": {"call"`, `"action": {"query": {"kinds": ["x"]}, "call"`, "kind name"},
		"an unknown name":   {`"body": "x"`, `"body": "{{nobody}}"`, "not an argument"},
		"an unknown filter": {`"body": "x"`, `"body": "{{me|shout}}"`, "does not exist"},
		"a missing format":  {`"summary": "s",`, `"summary": "s", "output": {"format": "nope"},`, "format \"nope\""},
		"a bad class":       {`"class": "live"`, `"class": "soon"`, "live or later"},
		"an open template":  {`"body": "x"`, `"body": "{{me"`, "does not close"},
		"a filter arg":      {`"body": "x"`, `"body": "{{me|keyed}}"`, "needs an argument"},
		"a reserved flag":   {`"summary": "s",`, `"summary": "s", "args": [{"name": "json", "kind": "switch"}],`, "every command has"},
		"a variadic in the middle": {`"summary": "s",`,
			`"summary": "s", "args": [{"name": "a", "kind": "positional", "type": "text", "variadic": true}, {"name": "b", "kind": "positional", "type": "text"}],`,
			"only the last positional"},
	}
	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("the minimal manifest does not parse: %v", err)
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			text := strings.Replace(minimal, c.from, c.to, 1)
			if text == minimal {
				t.Fatal("the case changed nothing")
			}
			_, err := Parse([]byte(text))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

// fakeEnv answers calls with a fixed reply, and records the request.
type fakeEnv struct {
	me      nostr.SecretKey
	reply   CallResult
	request CallRequest
	later   bool
	calls   int
}

func (f *fakeEnv) ResolveKey(_ context.Context, text string) (nostr.PubKey, error) {
	if pk, err := nostr.PubKeyFromHex(text); err == nil {
		return pk, nil
	}
	if prefix, value, err := nip19.Decode(text); err == nil && prefix == "npub" {
		return value.(nostr.PubKey), nil
	}
	return nostr.PubKey{}, errors.New("unknown key " + text)
}
func (f *fakeEnv) Me() nostr.PubKey { return f.me.Public() }
func (f *fakeEnv) Keyed(info []byte, input string) (string, error) {
	return KeyedValue(f.me, info, input)
}
func (f *fakeEnv) EventAuthor(context.Context, string) (nostr.PubKey, error) {
	return f.me.Public(), nil
}
func (f *fakeEnv) Name(pk nostr.PubKey) string { return "petname-" + pk.Hex()[:4] }
func (f *fakeEnv) Call(_ context.Context, _ nostr.PubKey, r CallRequest, later bool) (CallResult, error) {
	f.calls++
	f.request, f.later = r, later
	if later {
		return CallResult{Queued: true}, nil
	}
	return f.reply, nil
}

func runSpec(t *testing.T, id string, env *fakeEnv, words ...string) (string, string, error) {
	t.Helper()
	m := specManifests(t)[id]
	var out, errs bytes.Buffer
	err := Run(context.Background(), env, Installed{Manifest: m, Author: nostr.Generate().Public(), Name: id}, words,
		Stdio{In: strings.NewReader(""), Out: &out, Err: &errs})
	return out.String(), errs.String(), err
}

func TestExecRunSendsArgvAndShowsTheOutput(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: `{"exit":0,"stdout":"hello world\n","stderr":"","timed_out":false,"truncated":false}`}}
	out, _, err := runSpec(t, "exec", env, "run", "echo", "hello", "world")
	if err != nil {
		t.Fatal(err)
	}
	if env.request.Body != `{"argv": ["echo","hello","world"]}` {
		t.Errorf("the body is %s", env.request.Body)
	}
	if env.request.Capability != "exec" || env.request.Method != "EXEC" || env.request.Path != "/" || env.later {
		t.Errorf("the request is %+v, later %v", env.request, env.later)
	}
	if out != "hello world\n" {
		t.Errorf("the output is %q", out)
	}
}

func TestAVariadicJoinsToOneString(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: `{"columns":["n"],"rows":[[1]]}`}}
	if _, _, err := runSpec(t, "sqlite", env, "select", "1", "as", "n"); err != nil {
		t.Fatal(err)
	}
	if env.request.Body != `{"sql": "select 1 as n"}` || env.request.Method != "QUERY" || env.request.Path != "/main" {
		t.Errorf("the request is %+v", env.request)
	}
}

func TestSqliteShowsATable(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: `{"results":[{"columns":["id","name"],"rows":[[1,"ada"],[22,"grace hopper"]],"changes":0}]}`}}
	out, _, err := runSpec(t, "sqlite", env, "select * from people")
	if err != nil {
		t.Fatal(err)
	}
	want := "id  name\n1   ada\n22  grace hopper\n"
	if out != want {
		t.Errorf("the table is\n%s\nwant\n%s", out, want)
	}
}

func TestJSONWritesTheRecord(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: `{"exit":3,"stdout":"","stderr":"no\n"}`}}
	out, _, err := runSpec(t, "exec", env, "run", "--json", "false")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"exit":3`) || strings.Count(out, "\n") != 1 {
		t.Errorf("the JSON output is %q", out)
	}
}

func TestLaterQueuesTheCall(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate()}
	out, errs, err := runSpec(t, "exec", env, "run", "--later", "true")
	if err != nil {
		t.Fatal(err)
	}
	if !env.later || out != "" || !strings.Contains(errs, "queued") {
		t.Errorf("later %v, out %q, err %q", env.later, out, errs)
	}

	// A later class queues without the flag.
	env = &fakeEnv{me: nostr.Generate()}
	if _, _, err := runSpec(t, "exec", env, "start", "sleep", "1"); err != nil || !env.later {
		t.Errorf("start did not queue: %v", err)
	}
	if env.request.Body != `{"action": "start", "script": "sleep 1"}` {
		t.Errorf("the body is %s", env.request.Body)
	}
}

func TestARefusalFailsTheCommand(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Err: "access_denied"}}
	_, _, err := runSpec(t, "exec", env, "run", "id")
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("got %v", err)
	}
}

func TestArgumentsAreChecked(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate()}
	cases := map[string][]string{
		"missing <argv...>":  {"run"},
		"unknown flag --x":   {"run", "--x", "1"},
		"too many arguments": {"status", "a", "b"},
		"has no command":     {"fly"},
	}
	for want, words := range cases {
		_, _, err := runSpec(t, "exec", env, words...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: got %v, want %q", words, err, want)
		}
	}
	if env.calls != 0 {
		t.Errorf("a bad command still called the provider %d times", env.calls)
	}
}

func TestHelpListsTheCommands(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate()}
	for _, words := range [][]string{nil, {"help"}, {"--help"}} {
		out, _, err := runSpec(t, "exec", env, words...)
		if err != nil || !strings.Contains(out, "arcn exec run <argv...>") || !strings.Contains(out, "Start a script as a job") {
			t.Errorf("%v: %v\n%s", words, err, out)
		}
	}
	out, _, err := runSpec(t, "releases", env, "channel", "--help")
	if err != nil || !strings.Contains(out, "usage: arcn releases channel [<channel>]") || !strings.Contains(out, "default stable") {
		t.Errorf("command help: %v\n%s", err, out)
	}
	// sqlite has a root command with a positional, so no words show help.
	out, _, err = runSpec(t, "sqlite", env)
	if err != nil || !strings.Contains(out, "arcn sqlite <sql...>") || env.calls != 0 {
		t.Errorf("sqlite help: %v\n%s", err, out)
	}
}

func TestADefaultFillsAnAbsentArgument(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: "{}"}}
	if _, _, err := runSpec(t, "releases", env, "channel"); err != nil {
		t.Fatal(err)
	}
	if env.request.Body != `{"op": "channel", "channel": "stable"}` {
		t.Errorf("the body is %s", env.request.Body)
	}
}

func TestAValueCannotReachTheTerminal(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: "{\"stdout\":\"a\\u001b[2Jb\\u0007c\\n\",\"stderr\":\"\"}"}}
	out, _, err := runSpec(t, "exec", env, "run", "x")
	if err != nil {
		t.Fatal(err)
	}
	if out != "a[2Jbc\n" {
		t.Errorf("the output is %q", out)
	}
}

func TestKeyedValuesDifferByCapability(t *testing.T) {
	secret := nostr.Generate()
	author := nostr.Generate().Public()
	a, _ := KeyedValue(secret, keyedInfo(author, "journal", "page"), "hrs/a/b")
	again, _ := KeyedValue(secret, keyedInfo(author, "journal", "page"), "hrs/a/b")
	other, _ := KeyedValue(secret, keyedInfo(author, "files", "page"), "hrs/a/b")
	purpose, _ := KeyedValue(secret, keyedInfo(author, "journal", "kpi"), "hrs/a/b")
	if a != again || len(a) != 64 {
		t.Errorf("a keyed value is not stable: %s %s", a, again)
	}
	if a == other || a == purpose {
		t.Error("two capabilities, or two purposes, share a keyed value")
	}
}

func TestArgumentTypes(t *testing.T) {
	pk := nostr.Generate().Public()
	id := nostr.ID(pk)
	env := &fakeEnv{me: nostr.Generate()}
	cases := []struct {
		arg  Arg
		in   string
		want any
		err  string
	}{
		{Arg{Type: "key"}, nip19.EncodeNpub(pk), pk.Hex(), ""},
		{Arg{Type: "key"}, "nobody", nil, "unknown key"},
		{Arg{Type: "event"}, nip19.EncodeNevent(id, nil, nostr.ZeroPK), id.Hex(), ""},
		{Arg{Type: "event"}, nip19.EncodeNaddr(pk, 30023, "page", nil), "30023:" + pk.Hex() + ":page", ""},
		{Arg{Type: "event"}, nip19.EncodeNpub(pk), nil, "not an event"},
		{Arg{Type: "integer"}, "12", int64(12), ""},
		{Arg{Type: "integer"}, "twelve", nil, "whole number"},
		{Arg{Type: "lines"}, "3:9", "3:9", ""},
		{Arg{Type: "lines"}, "9:3", nil, "ends before"},
		{Arg{Type: "lines"}, ":", nil, "not a range"},
		{Arg{Type: "address", Pattern: "^[a-z]+/[a-z]+$"}, "a/b", "a/b", ""},
		{Arg{Type: "address", Pattern: "^[a-z]+/[a-z]+$"}, "a/b/c", nil, "does not match"},
		{Arg{Type: "path"}, "iface_test.go", nil, "never replaces"},
	}
	for _, c := range cases {
		got, err := convert(context.Background(), c.arg, c.in, env)
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
	os.WriteFile(path, []byte("hi"), 0o600)
	value, err := convert(context.Background(), Arg{Type: "file"}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := scope(func(string) (any, bool) { return value, true })
	tpl, _ := compile("{{f.name}} {{f.type}} {{f.size}} {{f.sha256|short}} {{f}}", nil)
	out, err := tpl.render(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "note.txt text/plain") || !strings.Contains(out, " 2 8f434346…") || !strings.HasSuffix(out, " aGk=") {
		t.Errorf("the file renders as %q", out)
	}
}

func TestAVariadicKeepsItsFlags(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate(), reply: CallResult{Body: `{"stdout":""}`}}
	if _, _, err := runSpec(t, "exec", env, "run", "--json", "sh", "-c", "echo --x"); err != nil {
		t.Fatal(err)
	}
	if env.request.Body != `{"argv": ["sh","-c","echo --x"]}` {
		t.Errorf("the body is %s", env.request.Body)
	}
}
