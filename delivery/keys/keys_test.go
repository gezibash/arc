package keys_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gezibash/arc/delivery/keys"
)

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	k := keys.Generate()

	if err := keys.Save(path, k); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key file has mode %v, want 0600", info.Mode().Perm())
	}

	again, err := keys.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Public != k.Public || again.Secret != k.Secret {
		t.Error("the loaded key differs from the saved key")
	}
	if again.Name() == "" || again.Name() != k.Name() {
		t.Errorf("the petname is %q, want %q", again.Name(), k.Name())
	}
}

func TestSaveRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := keys.Save(path, keys.Generate()); err != nil {
		t.Fatal(err)
	}
	if err := keys.Save(path, keys.Generate()); err != keys.ErrExists {
		t.Errorf("a second save gave %v, want ErrExists", err)
	}
}

func TestLoadRefusesAFileOthersCanRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := keys.Save(path, keys.Generate()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Load(path); err == nil {
		t.Error("a key file that others can read was loaded")
	}
}

func TestLoadWithoutAFile(t *testing.T) {
	if _, err := keys.Load(filepath.Join(t.TempDir(), "nothing")); err != keys.ErrNotFound {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
