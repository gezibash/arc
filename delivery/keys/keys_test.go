package keys_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr/nip19"

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

func TestAKeyFileHoldsHexNsecOrNcryptsec(t *testing.T) {
	k := keys.Generate()
	if got, err := keys.Parse(nip19.EncodeNsec(k.Secret)); err != nil || got.Public != k.Public {
		t.Errorf("nsec: %v", err)
	}
	sealed, err := keys.Encrypt(k, "correct horse")
	if err != nil || !strings.HasPrefix(sealed, "ncryptsec1") {
		t.Fatalf("encrypt: %q %v", sealed, err)
	}
	if _, err := keys.Parse(sealed); !errors.Is(err, keys.ErrEncrypted) {
		t.Errorf("an ncryptsec parsed without its passphrase: %v", err)
	}
	if got, err := keys.Decrypt(sealed, "correct horse"); err != nil || got.Public != k.Public {
		t.Errorf("decrypt: %v", err)
	}
	if _, err := keys.Decrypt(sealed, "wrong"); err == nil {
		t.Error("a wrong passphrase opened the key")
	}
	if _, err := keys.Parse("bunker://" + k.Public.Hex() + "?relay=wss://r"); !errors.Is(err, keys.ErrRemote) {
		t.Errorf("a bunker URI parsed as a key: %v", err)
	}
	if _, err := keys.Encrypt(k, ""); err == nil {
		t.Error("an empty passphrase sealed the key")
	}

	path := filepath.Join(t.TempDir(), "key")
	if err := keys.Write(path, sealed); err != nil {
		t.Fatal(err)
	}
	if text, err := keys.Read(path); err != nil || text != sealed {
		t.Errorf("read back %q %v", text, err)
	}
}
