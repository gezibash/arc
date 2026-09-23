package bundle_test

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gezibash/arc/bundle"
	"github.com/gezibash/arc/capability"
	"github.com/gezibash/arc/iface"
	"github.com/gezibash/arc/provider/host"
)

func TestInitWritesABundleThatServes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "weather-bot")

	files, err := bundle.Init(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{files.Arcfile, files.Manifest, files.Interface, files.Runtime} {
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
	pkg, err := capability.LoadFile(files.Manifest)
	if err != nil {
		t.Fatalf("the manifest is not a capability: %v", err)
	}

	fields, _ := pkg["capability"].(map[string]any)
	if fields["scheme"] != "weather-bot" {
		t.Errorf("scheme = %v, want the name of the directory", fields["scheme"])
	}
	if fields["title"] != "Weather Bot" {
		t.Errorf("title = %v", fields["title"])
	}

	// arc serve announces the interface in place of the manifest, and
	// refuses an interface that iface.Parse refuses.
	m := parseInterface(t, files.Interface)
	if m.ID != "weather-bot" {
		t.Errorf("id = %s, want the name of the directory", m.ID)
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
		parseInterface(t, files.Interface)
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
	root := write(t, `version = 1
[runtime]
type = "exec"
command = "/usr/bin/python3"
args = ["-u", "server.py"]
[manifest]
path = "./manifest.json"
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
	root := write(t, `version = 1
[runtime]
type = "exec"
command = "./run.sh"
args = ["-u", "server.py", "--greeting", "hello world", "a+b&c=%41"]
[manifest]
path = "./manifest.json"
`)
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	held, err := bundle.Load(filepath.Join(root, bundle.ArcfileName))
	if err != nil {
		t.Fatal(err)
	}
	_, args, _, err := host.ParseServeURI(held.ServeURI())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"-u", "server.py", "--greeting", "hello world", "a+b&c=%41"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestAnArcfileNamesThePathsFromItsOwnDirectory(t *testing.T) {
	root := write(t, `version = 1
[runtime]
type = "exec"
command = "./run.sh"
cwd = "."
[manifest]
path = "./manifest.json"
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
		"another version": `version = 2
[runtime]
type = "exec"
command = "./run.sh"
[manifest]
path = "./manifest.json"
`,
		"another runtime": `version = 1
[runtime]
type = "docker"
command = "./run.sh"
[manifest]
path = "./manifest.json"
`,
		"no command": `version = 1
[runtime]
type = "exec"
[manifest]
path = "./manifest.json"
`,
		"no manifest": `version = 1
[runtime]
type = "exec"
command = "./run.sh"
`,
	}

	for name, arcfile := range cases {
		if _, err := bundle.Load(filepath.Join(write(t, arcfile), bundle.ArcfileName)); err == nil {
			t.Errorf("%s: the Arcfile loaded", name)
		}
	}
}

func TestRefusesAMissingManifest(t *testing.T) {
	root := t.TempDir()
	arcfile := filepath.Join(root, bundle.ArcfileName)

	body := "version = 1\n[runtime]\ntype = \"exec\"\ncommand = \"./run.sh\"\n[manifest]\npath = \"./manifest.json\"\n"
	if err := os.WriteFile(arcfile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := bundle.Load(arcfile); err != bundle.ErrManifestAbsent {
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
