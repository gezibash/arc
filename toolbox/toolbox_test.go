package toolbox_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/capability"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/sealedbox"
	"github.com/gezibash/arc/toolbox"
)

// signedEcho is a capability with a command line, signed by one citizen.
func signedEcho(t *testing.T, me *identity.Identity) map[string]any {
	t.Helper()

	document := map[string]any{
		"release": map[string]any{"version": "0.1.0", "channel": "stable"},
		"capability": map[string]any{
			"id": "primary", "kind": "compute", "scheme": "echo",
			"title": "Echo", "summary": "Answers with what it hears.",
			"invocation": map[string]any{"mode": "request_reply", "method": "ECHO", "path": "/"},
			"interfaces": map[string]any{"cli": map[string]any{
				"version": 1, "namespace": "echo", "summary": "Say something.",
				"commands": []any{
					map[string]any{
						"path": []any{}, "summary": "Say something.",
						"args": []any{map[string]any{
							"name": "words", "kind": "positional", "required": true, "variadic": true,
						}},
						"input": map[string]any{"source": "arg", "name": "words", "join_with": " "},
					},
					map[string]any{
						"path": []any{"twice"},
						"args": []any{
							map[string]any{"name": "words", "kind": "positional", "required": true, "variadic": true},
							map[string]any{"name": "from", "kind": "option", "flag": "--from", "type": "string"},
						},
						"input": map[string]any{"source": "template", "template": "{{from}} says {{words}}"},
					},
				},
			}},
		},
	}

	signed, err := capability.Sign(me, document)
	if err != nil {
		t.Fatal(err)
	}

	// The package travels as JSON, and its numbers keep their digits.
	encoded, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()

	var held map[string]any
	if err := decoder.Decode(&held); err != nil {
		t.Fatal(err)
	}
	return held
}

func testStore(t *testing.T) (*toolbox.Store, *identity.Identity, *identity.Identity) {
	t.Helper()

	owner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return &toolbox.Store{Dir: filepath.Join(t.TempDir(), "arc")}, owner, provider
}

func TestInstallAndList(t *testing.T) {
	store, owner, provider := testStore(t)
	signed := signedEcho(t, provider)

	record, err := store.Install(owner.PublicKey, signed, toolbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if record["command"] != "echo" {
		t.Errorf("command = %v", record["command"])
	}
	if record["usage"] != "arc echo <subcommand>" {
		t.Errorf("usage = %v", record["usage"])
	}
	if record["signer_public_key"] != provider.EncodePublicKey() {
		t.Errorf("signer = %v", record["signer_public_key"])
	}

	held, err := store.Get(owner.PublicKey, "echo")
	if err != nil {
		t.Fatal(err)
	}
	if held["package_hash"] != signed["package_hash"] {
		t.Error("the install holds another package")
	}

	if !store.Installed(owner.PublicKey, "echo") {
		t.Error("the command is not installed")
	}
	if store.Installed(owner.PublicKey, "other") {
		t.Error("a command that was never installed answers")
	}

	// Another owner holds nothing.
	other, _ := identity.Generate()
	if tools, _ := store.List(other.PublicKey); len(tools) != 0 {
		t.Errorf("another owner holds %v", tools)
	}
}

func TestInstallUnderAnotherName(t *testing.T) {
	store, owner, provider := testStore(t)

	record, err := store.Install(owner.PublicKey, signedEcho(t, provider),
		toolbox.Options{Command: "Say It"})
	if err != nil {
		t.Fatal(err)
	}
	if record["command"] != "say-it" {
		t.Errorf("command = %v", record["command"])
	}
}

func TestInstallRefusesWhatItMust(t *testing.T) {
	store, owner, provider := testStore(t)
	signed := signedEcho(t, provider)

	if _, err := store.Install(owner.PublicKey, signed, toolbox.Options{Command: "keys"}); err != toolbox.ErrReserved {
		t.Errorf("a reserved name gave %v", err)
	}

	// A capability without a command line is not installable.
	plain, err := capability.Sign(provider, map[string]any{
		"release": map[string]any{"version": "1.0.0", "channel": "stable"},
		"capability": map[string]any{
			"id": "primary", "kind": "compute", "scheme": "exec", "title": "T", "summary": "S",
			"invocation": map[string]any{"method": "EXEC", "path": "/"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(owner.PublicKey, plain, toolbox.Options{}); err != toolbox.ErrNotInstallable {
		t.Errorf("a capability without a command line gave %v", err)
	}

	// Another signer may not take a command that stands.
	if _, err := store.Install(owner.PublicKey, signed, toolbox.Options{}); err != nil {
		t.Fatal(err)
	}

	stranger, _ := identity.Generate()
	if _, err := store.Install(owner.PublicKey, signedEcho(t, stranger), toolbox.Options{}); err != toolbox.ErrSignerConflict {
		t.Errorf("another signer gave %v", err)
	}

	// The same signer replaces its own install.
	if _, err := store.Install(owner.PublicKey, signedEcho(t, provider), toolbox.Options{}); err != nil {
		t.Errorf("the same signer could not install again: %v", err)
	}
}

func TestPinAndRemove(t *testing.T) {
	store, owner, provider := testStore(t)
	if _, err := store.Install(owner.PublicKey, signedEcho(t, provider), toolbox.Options{}); err != nil {
		t.Fatal(err)
	}

	record, err := store.SetPinned(owner.PublicKey, "echo", true)
	if err != nil {
		t.Fatal(err)
	}
	if pinned, _ := record["pinned"].(bool); !pinned {
		t.Error("the command is not pinned")
	}

	if err := store.Uninstall(owner.PublicKey, "echo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(owner.PublicKey, "echo"); err != toolbox.ErrNotFound {
		t.Errorf("the command stands after the removal: %v", err)
	}
	if _, err := store.SetPinned(owner.PublicKey, "echo", true); err != toolbox.ErrNotFound {
		t.Errorf("pinning a command that is not there gave %v", err)
	}
}

func TestTrust(t *testing.T) {
	store, owner, provider := testStore(t)
	signer := provider.EncodePublicKey()

	if state, err := store.TrustState(owner.PublicKey, signer); err != nil || state != "" {
		t.Errorf("a signer without a decision gave %q, %v", state, err)
	}

	if _, err := store.Trust(owner.PublicKey, signer, toolbox.Allowed, "a note"); err != nil {
		t.Fatal(err)
	}
	if state, _ := store.TrustState(owner.PublicKey, signer); state != toolbox.Allowed {
		t.Errorf("state = %q", state)
	}

	// A later decision replaces the first, and keeps its time.
	first, _ := store.Signers(owner.PublicKey)
	if _, err := store.Trust(owner.PublicKey, signer, toolbox.Denied, ""); err != nil {
		t.Fatal(err)
	}

	signers, _ := store.Signers(owner.PublicKey)
	if len(signers) != 1 || signers[0].State != toolbox.Denied {
		t.Fatalf("the signers are %+v", signers)
	}
	if signers[0].FirstTrustedAt != first[0].FirstTrustedAt {
		t.Error("the time of the first decision changed")
	}

	if _, err := store.Trust(owner.PublicKey, "short", toolbox.Allowed, ""); err == nil {
		t.Error("a signer that is not a key passed")
	}
	if _, err := store.Trust(owner.PublicKey, signer, "maybe", ""); err == nil {
		t.Error("a decision that is neither allowed nor denied passed")
	}
}

func TestBuildsOneRequestFromACommandLine(t *testing.T) {
	store, owner, provider := testStore(t)
	record, err := store.Install(owner.PublicKey, signedEcho(t, provider), toolbox.Options{})
	if err != nil {
		t.Fatal(err)
	}

	got, err := toolbox.Build(record, []string{"hello", "world"}, toolbox.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "hello world" {
		t.Errorf("body = %q", got.Body)
	}
	if got.Meta["method"] != "ECHO" || got.Meta["path"] != "/" {
		t.Errorf("meta = %v", got.Meta)
	}

	// A subcommand takes its own arguments, and renders its template.
	got, err = toolbox.Build(record, []string{"twice", "--from", "ada", "the", "words"}, toolbox.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "ada says the words" {
		t.Errorf("body = %q", got.Body)
	}
}

func TestRefusesACommandLineThatDoesNotFit(t *testing.T) {
	store, owner, provider := testStore(t)
	record, err := store.Install(owner.PublicKey, signedEcho(t, provider), toolbox.Options{})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]string{
		"no words":                   {},
		"an unknown option":          {"--colour", "red", "hello"},
		"an option without a value":  {"twice", "--from"},
		"a subcommand without words": {"twice", "--from", "ada"},
	}

	for name, argv := range cases {
		if _, err := toolbox.Build(record, argv, toolbox.Context{}); err == nil {
			t.Errorf("%s: the line passed", name)
		}
	}
}

// The filters of a template render keys and sealed bodies.
func TestTheFiltersOfATemplate(t *testing.T) {
	me, _ := identity.Generate()
	peer, _ := identity.Generate()

	command := map[string]any{
		"path": []any{"send"},
		"args": []any{
			map[string]any{"name": "to", "kind": "positional", "required": true},
			map[string]any{"name": "body", "kind": "positional", "required": true, "variadic": true},
		},
		"input": map[string]any{
			"source":   "template",
			"template": "send --to {{to|pubkey}}\n{{body|seal:to}}",
		},
	}

	install := map[string]any{
		"command": "dm",
		"capability": map[string]any{
			"invocation": map[string]any{"method": "RAW", "path": "/"},
			"interfaces": map[string]any{"cli": map[string]any{
				"version": 1, "namespace": "dm", "commands": []any{command},
			}},
		},
	}

	context := toolbox.Context{
		Identity: me,
		Resolve: func(query string) ([][]byte, error) {
			if query == "friend" {
				return [][]byte{peer.PublicKey}, nil
			}
			return nil, nil
		},
	}

	got, err := toolbox.Build(install, []string{"send", "friend", "the", "message"}, context)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(got.Body, "\n")
	if len(lines) != 2 {
		t.Fatalf("body = %q", got.Body)
	}
	if lines[0] != "send --to "+peer.EncodePublicKey() {
		t.Errorf("the first line is %q", lines[0])
	}

	token, found := strings.CutPrefix(lines[1], "sealed-v1:")
	if !found {
		t.Fatalf("the body is not sealed: %q", lines[1])
	}

	raw, err := base64Decode(token)
	if err != nil {
		t.Fatal(err)
	}

	opened, err := sealedbox.Open(peer, raw)
	if err != nil {
		t.Fatalf("the reader cannot open it: %v", err)
	}
	if string(opened) != "the message" {
		t.Errorf("the body is %q", opened)
	}

	// A name that answers to no one fails.
	if _, err := toolbox.Build(install, []string{"send", "nobody", "x"}, context); err == nil {
		t.Error("a name that answers to no one passed")
	}
}

func TestUsageLines(t *testing.T) {
	_, _, provider := testStore(t)
	signed := signedEcho(t, provider)
	fields, _ := signed["capability"].(map[string]any)

	if got := toolbox.Usage("echo", fields); got != "arc echo <subcommand>" {
		t.Errorf("usage = %q", got)
	}

	root := toolbox.RootCommand(fields)
	if got := toolbox.CommandUsage("echo", root); got != "arc echo <words...>" {
		t.Errorf("the root usage is %q", got)
	}

	subcommands := toolbox.Subcommands(fields)
	if len(subcommands) != 1 {
		t.Fatalf("the capability holds %d subcommands", len(subcommands))
	}
	if got := toolbox.Label(subcommands[0]); got != "twice" {
		t.Errorf("label = %q", got)
	}
	if got := toolbox.CommandUsage("echo", subcommands[0]); got != "arc echo twice <words...> [--from <from>]" {
		t.Errorf("the subcommand usage is %q", got)
	}
}

func TestReservedNames(t *testing.T) {
	for _, name := range []string{"keys", "serve", "install", "trust", "help"} {
		if !toolbox.Reserved(name) {
			t.Errorf("%s is not held back", name)
		}
	}
	if toolbox.Reserved("echo") {
		t.Error("echo is held back")
	}
	if got := toolbox.NormalizeCommand("My Tool!"); got != "my-tool" {
		t.Errorf("the name became %q", got)
	}
}
