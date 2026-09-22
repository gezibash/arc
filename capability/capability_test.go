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
