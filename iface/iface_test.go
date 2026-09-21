package iface

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip19"
	"github.com/gezibash/arc/delivery/store"
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
// fakeNet stands in for the relays and the mail between citizens.
type fakeNet struct {
	relays map[string]*store.Store
	inbox  map[nostr.PubKey][]nostr.Event
}

func newNet() *fakeNet {
	return &fakeNet{relays: map[string]*store.Store{}, inbox: map[nostr.PubKey][]nostr.Event{}}
}

func (n *fakeNet) relay(t *testing.T, url string) *store.Store {
	if s := n.relays[url]; s != nil {
		return s
	}
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	n.relays[url] = s
	return s
}

type fakeEnv struct {
	t     *testing.T
	net   *fakeNet
	me    nostr.SecretKey
	store *store.Store
	live  chan nostr.Event
	// fetchedIDs counts the events that fetches by ID asked for.
	fetchedIDs int
	reply      CallResult
	request    CallRequest
	later      bool
	calls      int
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
func (f *fakeEnv) Me() nostr.PubKey   { return f.me.Public() }
func (f *fakeEnv) Keyer() nostr.Keyer { return keyer.NewPlainKeySigner(f.me) }
func (f *fakeEnv) Publish(_ context.Context, events []nostr.Event, relays []string) error {
	for _, event := range events {
		s := f.store
		if relays != nil {
			s = f.net.relay(f.t, relays[0])
		}
		result, err := s.Save(event)
		if err != nil {
			return err
		}
		if result.Outcome == store.Refused {
			return errors.New(result.Reason)
		}
		if f.live != nil {
			f.live <- event
		}
	}
	return nil
}
func (f *fakeEnv) Fetch(_ context.Context, filter nostr.Filter, relays []string) ([]nostr.Event, error) {
	f.fetchedIDs += len(filter.IDs)
	if relays != nil {
		return f.net.relay(f.t, relays[0]).Query(filter), nil
	}
	return f.store.Query(filter), nil
}
func (f *fakeEnv) Watch(context.Context, nostr.Filter, []string) (<-chan nostr.Event, error) {
	if f.live == nil {
		return nil, errors.New("no relay to watch")
	}
	return f.live, nil
}
func (f *fakeEnv) SendPrivate(_ context.Context, to nostr.PubKey, kind nostr.Kind, content string, tags nostr.Tags) error {
	rumor := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), Content: content, Tags: tags, PubKey: f.me.Public()}
	rumor.ID = rumor.GetID()
	f.net.inbox[to] = append(f.net.inbox[to], rumor)
	f.net.inbox[f.me.Public()] = append(f.net.inbox[f.me.Public()], rumor)
	return nil
}
func (f *fakeEnv) Private(_ context.Context, kinds []nostr.Kind) ([]nostr.Event, error) {
	var out []nostr.Event
	for _, rumor := range f.net.inbox[f.me.Public()] {
		if slices.Contains(kinds, rumor.Kind) {
			out = append(out, rumor)
		}
	}
	return out, nil
}
func (f *fakeEnv) Keyed(info []byte, input string) (string, error) {
	root, err := KeyedRoot(f.me)
	if err != nil {
		return "", err
	}
	return KeyedValue(root, info, input)
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
	secret, _ := KeyedRoot(nostr.Generate())
	author := nostr.Generate().Public()
	a, _ := KeyedValue(secret, keyedInfo(author, "journal", "page"), "hrs/a/b")
	again, _ := KeyedValue(secret, keyedInfo(author, "journal", "page"), "hrs/a/b")
	other, _ := KeyedValue(secret, keyedInfo(author, "files", "page"), "hrs/a/b")
	purpose, _ := KeyedValue(secret, keyedInfo(author, "journal", "kpi"), "hrs/a/b")
	if a != again || len(a) != 22 {
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

// The manifest files in the repository are the manifests of the spec.
func TestTheManifestFilesAreTheSpec(t *testing.T) {
	spec := specManifests(t)
	paths, _ := filepath.Glob("../manifests/*.json")
	providers, _ := filepath.Glob("../cmd/*-provider/interface.json")
	paths = append(paths, providers...)
	if len(paths) != 7 {
		t.Fatalf("found %d manifest files, want 7", len(paths))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m, err := Parse(data)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !reflect.DeepEqual(m, spec[m.ID]) {
			t.Errorf("%s is not the manifest of the spec", path)
		}
	}
}

func TestKeyedListsKeepTheirWordsApart(t *testing.T) {
	env := &fakeEnv{me: nostr.Generate()}
	r := &run{env: env, in: Installed{Manifest: &Manifest{ID: "x"}}, values: Values{"a": "ab", "b": "c", "c": "a", "d": "bc"}}
	one, _ := compile("{{a+b|keyed:p}}", nil)
	two, _ := compile("{{c+d|keyed:p}}", nil)
	x, _ := one.render(r.scope, r, false)
	y, _ := two.render(r.scope, r, false)
	if x == y || len(x) != 22 {
		t.Errorf("ab+c and a+bc key to %s and %s", x, y)
	}
}
