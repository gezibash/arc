package capability_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gezibash/arc/capability"
)

func TestLoadsTOMLAndJSONTheSame(t *testing.T) {
	json := `{"release":{"version":"1.0.0","channel":"stable"},
	  "capability":{"id":"primary","kind":"compute","scheme":"exec","title":"T","summary":"S",
	  "invocation":{"method":"EXEC","path":"/"}}}`

	toml := "[release]\nversion = \"1.0.0\"\nchannel = \"stable\"\n\n" +
		"[capability]\nid = \"primary\"\nkind = \"compute\"\nscheme = \"exec\"\n" +
		"title = \"T\"\nsummary = \"S\"\n\n[capability.invocation]\nmethod = \"EXEC\"\npath = \"/\"\n"

	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.json"), json)
	write(t, filepath.Join(dir, "a.toml"), toml)
	fromJSON, err := capability.LoadFile(filepath.Join(dir, "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	fromTOML, err := capability.LoadFile(filepath.Join(dir, "a.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJSON, fromTOML) {
		t.Errorf("the two files gave different packages:\n%v\n%v", fromJSON, fromTOML)
	}
}

// A manifest of the older stack can hold a command line, a mode and a
// stream. No caller reads them. The manifest still loads, and none of them
// reaches the package.
func TestIgnoresTheCommandLineOfTheOlderStack(t *testing.T) {
	plain := `{"release":{"version":"1.0.0","channel":"stable"},
	  "capability":{"id":"primary","kind":"compute","scheme":"echo","title":"T","summary":"S",
	  "invocation":{"method":"ECHO","path":"/"}}}`

	older := `{"release":{"version":"1.0.0","channel":"stable"},
	  "capability":{"id":"primary","kind":"compute","scheme":"echo","title":"T","summary":"S",
	  "invocation":{"mode":"stream","method":"ECHO","path":"/","stream":{"tty":true}},
	  "cli":{"namespace":"echo","args":[{"name":"words"}]}},
	  "interfaces":{"cli":{"version":1,"namespace":"echo",
	    "commands":[{"path":[],"invoke":{"mode":"events","topics":["news"]}}]}}}`

	dir := t.TempDir()
	write(t, filepath.Join(dir, "plain.json"), plain)
	write(t, filepath.Join(dir, "older.json"), older)
	fromPlain, err := capability.LoadFile(filepath.Join(dir, "plain.json"))
	if err != nil {
		t.Fatal(err)
	}
	fromOlder, err := capability.LoadFile(filepath.Join(dir, "older.json"))
	if err != nil {
		t.Fatalf("the manifest of the older stack did not load: %v", err)
	}
	if !reflect.DeepEqual(fromPlain, fromOlder) {
		t.Errorf("the older command line reached the package:\n%v\n%v", fromPlain, fromOlder)
	}
}

func TestRefusesAFileThatIsNotAPackage(t *testing.T) {
	dir := t.TempDir()

	for name, body := range map[string]string{
		"no capability": `{"release":{"version":"1.0.0"}}`,
		"no title":      `{"capability":{"id":"primary","kind":"compute","scheme":"exec"}}`,
		"not JSON":      `not json`,
	} {
		path := filepath.Join(dir, "bad.json")
		write(t, path, body)

		if _, err := capability.LoadFile(path); err == nil {
			t.Errorf("%s: the file passed", name)
		}
	}

	write(t, filepath.Join(dir, "bad.yaml"), "x: 1")
	if _, err := capability.LoadFile(filepath.Join(dir, "bad.yaml")); err != capability.ErrUnsupportedFile {
		t.Error("a file that is not JSON or TOML passed")
	}
}
