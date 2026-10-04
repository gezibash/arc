package bundle_test

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gezibash/arc/adapters/provider/host"
	"github.com/gezibash/arc/runtime/bundle"
	"github.com/gezibash/arc/runtime/capability"
	"github.com/gezibash/arc/runtime/iface"
)

func TestInitWritesABundleThatServes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "weather-bot")

	files, err := bundle.Init(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{files.Arcfile, files.Manifest, files.Runtime} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s is missing: %v", path, err)
		}
	}

	info, err := os.Stat(files.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("the runtime is not executable: %v", info.Mode())
	}

	// The manifest that Init writes must be a capability that ARC serves.
	_, err = capability.LoadProvider(files.Manifest)
	if err != nil {
		t.Fatalf("the manifest is not a capability: %v", err)
	}

	m := parseInterface(t, files.Manifest)
	if m.ID != "weather-bot" {
		t.Errorf("scheme = %v, want the name of the directory", m.ID)
	}
	if m.Title != "Weather Bot" {
		t.Errorf("title = %v", m.Title)
	}

	// The bundle resolves into an address that the runtime reads.
	address, held, err := bundle.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if held == nil {
		t.Fatal("the directory did not read as a bundle")
	}

	if !strings.HasPrefix(address, "exec://"+filepath.Join(root, "run.sh")+"?") {
		t.Errorf("address = %s", address)
	}

	query, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	if got := query.Query().Get("manifest"); got != files.Manifest {
		t.Errorf("manifest = %s, want %s", got, files.Manifest)
	}
}

// An interface id starts with a letter and has at most 64 characters. A
// directory name need not.
func TestInitWritesAnInterfaceForAnyDirectoryName(t *testing.T) {
	for _, name := range []string{"2048", "My App!", strings.Repeat("long-name-", 10)} {
		files, err := bundle.Init(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		parseInterface(t, files.Manifest)
	}
}

// The runtime must read back the exact path of the program, whatever the
// name of the directory.
func TestTheAddressKeepsThePathOfTheProgram(t *testing.T) {
	base := t.TempDir()

	for _, name := range []string{"my bots", "bot#1", "100%", "a%20b", "what?"} {
		root := filepath.Join(base, name)
		if _, err := bundle.Init(root); err != nil {
			t.Fatal(err)
		}
		held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
		if err != nil {
			t.Fatal(err)
		}

		path, _, manifest, _, err := host.ParseServeURI(held.ServeURI())
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := filepath.Join(root, "run.sh"); path != want {
			t.Errorf("%s: program = %s, want %s", name, path, want)
		}
		if want := filepath.Join(root, "manifest.json"); manifest != want {
			t.Errorf("%s: manifest = %s, want %s", name, manifest, want)
		}
	}
}

func TestInitWritesAManifestForAnUnusualName(t *testing.T) {
	titles := map[string]string{
		// The letter é takes two bytes. The title must keep both.
		"émile-bot": "Émile Bot",
		// Go quoting writes DEL as \x7f, and JSON has no such escape.
		"del\x7f-bot": "Del\x7f Bot",
	}

	for name, want := range titles {
		files, err := bundle.Init(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}

		_, err = capability.LoadProvider(files.Manifest)
		if err != nil {
			t.Errorf("%q: the manifest is not a capability: %v", name, err)
			continue
		}

		m := parseInterface(t, files.Manifest)
		if m.Title != want {
			t.Errorf("%q: title = %q, want %q", name, m.Title, want)
		}

		// arc serve announces the interface, so it must parse too.
		if m := parseInterface(t, files.Manifest); m.Title != want {
			t.Errorf("%q: the title of the interface = %q, want %q", name, m.Title, want)
		}
	}
}

func TestInitRefusesToWriteOverAFile(t *testing.T) {
	for _, name := range []string{bundle.ArcfileName, bundle.ManifestName, bundle.InterfaceName, bundle.RuntimeName} {
		root := t.TempDir()
		path := filepath.Join(root, name)

		if err := os.WriteFile(path, []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := bundle.Init(root); err == nil {
			t.Errorf("Init wrote over the %s that was already there", name)
		}
		if body, _ := os.ReadFile(path); string(body) != "mine\n" {
			t.Errorf("Init changed the %s that was already there", name)
		}
	}
}

// parseInterface reads an interface.json as arc serve reads it.
func parseInterface(t *testing.T, path string) *iface.Manifest {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := iface.Parse(body)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// write puts an Arcfile and a manifest in a new directory.
func write(t *testing.T, arcfile string) string {
	t.Helper()
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, bundle.ArcfileName), []byte(arcfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, bundle.ManifestName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCarriesTheArgumentsOfTheRuntime(t *testing.T) {
	root := write(t, `version = 2
[serve]
command = "/usr/bin/python3"
args = ["-u", "server.py"]
manifest = "./manifest.json"
`)

	address, held, err := bundle.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if held.Command != "/usr/bin/python3" {
		t.Errorf("command = %s", held.Command)
	}

	query, _ := url.Parse(address)
	if got := query.Query().Get("args"); got != `["-u","server.py"]` {
		t.Errorf("args = %s", got)
	}
}

func TestTheRuntimeGetsEachArgumentOfTheArcfile(t *testing.T) {
	root := write(t, `version = 2
[serve]
command = "./run.sh"
args = ["-u", "server.py", "--greeting", "hello world", "a+b&c=%41"]
manifest = "./manifest.json"
`)
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	_, args, _, _, err := host.ParseServeURI(held.ServeURI())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"-u", "server.py", "--greeting", "hello world", "a+b&c=%41"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestAnArcfileNamesThePathsFromItsOwnDirectory(t *testing.T) {
	root := write(t, `version = 2
[serve]
command = "./run.sh"
cwd = "."
manifest = "./manifest.json"
`)

	held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	if held.Command != filepath.Join(root, "run.sh") {
		t.Errorf("command = %s", held.Command)
	}
	if held.Manifest != filepath.Join(root, bundle.ManifestName) {
		t.Errorf("manifest = %s", held.Manifest)
	}
	if held.Cwd != root {
		t.Errorf("cwd = %s", held.Cwd)
	}
}

func TestRefusesAnArcfileThatSaysTooLittle(t *testing.T) {
	cases := map[string]string{
		"no version": `[serve]
command = "./run.sh"
manifest = "./manifest.json"
`,
		"a later version": `version = 3
[serve]
command = "./run.sh"
manifest = "./manifest.json"
`,
		"no command": `version = 2
[serve]
manifest = "./manifest.json"
`,
		"no manifest": `version = 2
[serve]
command = "./run.sh"
`,
		"a misspelt field": `version = 2
[serve]
command = "./run.sh"
manifest = "./manifest.json"
protcol = "http"
`,
		"a protocol that is not a name": `version = 2
[serve]
command = "./run.sh"
manifest = "./manifest.json"
protocol = "../http"
`,
		"a use with no app": `version = 2
[serve]
command = "./run.sh"
manifest = "./manifest.json"
[uses]
geo = ""
`,
		"a use name with capitals": `version = 2
[serve]
command = "./run.sh"
manifest = "./manifest.json"
[uses]
Geo = "geo"
`,
	}

	for name, arcfile := range cases {
		if _, err := bundle.Load(filepath.Join(write(t, arcfile), bundle.ArcfileName)); err == nil {
			t.Errorf("%s: the Arcfile loaded", name)
		}
	}
}

// An Arcfile of version 1 stops with the steps that rewrite it.
func TestRefusesVersionOneWithTheRewrite(t *testing.T) {
	root := write(t, `version = 1
[runtime]
type = "exec"
command = "./run.sh"
[manifest]
path = "./manifest.json"
`)
	_, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if !errors.Is(err, bundle.ErrVersion1) || !strings.Contains(err.Error(), "[serve]") {
		t.Fatalf("error = %v, want the rewrite to version 2", err)
	}
}

// A command without a slash comes from PATH, as in a shell. A command with
// a slash is a path from the directory of the program.
func TestABareCommandComesFromPath(t *testing.T) {
	root := write(t, `version = 2
[serve]
command = "sh"
manifest = "./manifest.json"
`)
	held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := exec.LookPath("sh"); held.Command != want {
		t.Errorf("command = %s, want %s", held.Command, want)
	}

	root = write(t, `version = 2
[serve]
command = "no-such-arc-command"
manifest = "./manifest.json"
`)
	_, err = bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("error = %v, want a command that is not on PATH", err)
	}
}

// A protocol other than stdio runs the translator arc-<protocol>, and the
// translator runs the program.
func TestAProtocolRunsItsTranslator(t *testing.T) {
	bin := t.TempDir()
	translator := filepath.Join(bin, "arc-fake")
	if err := os.WriteFile(translator, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := write(t, `version = 2
[serve]
command = "./server"
args = ["--port-from-env"]
protocol = "fake"
manifest = "./manifest.json"
`)
	held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	if held.Command != translator || held.Protocol != "fake" {
		t.Errorf("command = %s, protocol = %s", held.Command, held.Protocol)
	}
	if want := []string{filepath.Join(root, "server"), "--port-from-env"}; !slices.Equal(held.Args, want) {
		t.Errorf("args = %q, want %q", held.Args, want)
	}

	root = write(t, `version = 2
[serve]
command = "./server"
protocol = "no-such-protocol"
manifest = "./manifest.json"
`)
	_, err = bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err == nil || !strings.Contains(err.Error(), "arc-no-such-protocol") {
		t.Errorf("error = %v, want the name of the missing translator", err)
	}
}

func TestStdioIsTheDefaultProtocol(t *testing.T) {
	root := write(t, `version = 2
[serve]
command = "./run.sh"
manifest = "./manifest.json"
`)
	held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	if held.Protocol != bundle.Stdio || held.Command != filepath.Join(root, "run.sh") {
		t.Errorf("protocol = %s, command = %s", held.Protocol, held.Command)
	}
	if held.Allow != nil || held.Uses != nil {
		t.Errorf("allow = %v, uses = %v, want neither", held.Allow, held.Uses)
	}
}

// [uses] gives each name an environment variable. An empty [uses] allows no
// call, and a missing one allows every installed app.
func TestUsesNamesEachInstalledApp(t *testing.T) {
	root := write(t, `version = 2
[serve]
command = "./run.sh"
manifest = "./manifest.json"
allow = ["npub1example", "aaaa"]
[uses]
warehouse = "warehouse"
geo-data = "geo"
`)
	held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	want := []bundle.Use{{Name: "geo-data", Installed: "geo"}, {Name: "warehouse", Installed: "warehouse"}}
	if !slices.Equal(held.Uses, want) {
		t.Errorf("uses = %v, want %v", held.Uses, want)
	}
	if got := held.Uses[0].Env(); got != "ARC_USE_GEO_DATA" {
		t.Errorf("env = %s", got)
	}
	if !slices.Equal(held.Allow, []string{"npub1example", "aaaa"}) {
		t.Errorf("allow = %v", held.Allow)
	}

	root = write(t, `version = 2
[serve]
command = "./run.sh"
manifest = "./manifest.json"
[uses]
`)
	held, err = bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	if held.Uses == nil || len(held.Uses) != 0 {
		t.Errorf("uses = %#v, want an empty list that allows no call", held.Uses)
	}
}

func TestRefusesAMissingManifest(t *testing.T) {
	root := t.TempDir()
	arcfile := filepath.Join(root, bundle.ArcfileName)

	body := "version = 2\n[serve]\ncommand = \"./run.sh\"\nmanifest = \"./manifest.json\"\n"
	if err := os.WriteFile(arcfile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := bundle.Load(arcfile); !errors.Is(err, bundle.ErrManifestAbsent) {
		t.Errorf("error = %v, want the missing manifest", err)
	}
}

func TestAnAddressStaysAsItIs(t *testing.T) {
	for _, address := range []string{
		"exec:///usr/bin/dm?manifest=/tmp/one.json",
		"http://127.0.0.1:9000/",
		"/a/path/that/is/not/there",
	} {
		got, held, err := bundle.Resolve(address)
		if err != nil {
			t.Fatalf("%s: %v", address, err)
		}
		if held != nil || got != address {
			t.Errorf("%s became %s", address, got)
		}
	}
}
