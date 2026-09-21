package identity_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/identity"
)

// testStore returns a store with two keys, and a working directory with no
// arc.key. ARC_KEY is unset.
func testStore(t *testing.T) (*identity.Store, *identity.Identity, *identity.Identity) {
	t.Helper()
	t.Setenv("ARC_KEY", "")
	t.Chdir(t.TempDir())

	store := &identity.Store{Dir: t.TempDir()}
	first, err := store.Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return store, first, second
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNoSelectorIsNoDefault(t *testing.T) {
	store, _, _ := testStore(t)

	if _, source, err := store.Active(); !errors.Is(err, identity.ErrNoDefault) || source != identity.FromDefault {
		t.Fatalf("source %q, err %v", source, err)
	}
}

func TestEachSelectorSaysWhereTheKeyCameFrom(t *testing.T) {
	store, first, second := testStore(t)
	if err := store.SetDefault(first.Name()); err != nil {
		t.Fatal(err)
	}

	me, source, err := store.Active()
	if err != nil || me.Name() != first.Name() || source != identity.FromDefault {
		t.Fatalf("default: %v %q %v", me, source, err)
	}

	write(t, "arc.key", second.Name()+"\n")
	if me, source, err = store.Active(); err != nil || me.Name() != second.Name() || source != identity.FromDirectory {
		t.Fatalf("arc.key: %v %q %v", me, source, err)
	}

	t.Setenv("ARC_KEY", first.Name())
	if me, source, err = store.Active(); err != nil || me.Name() != first.Name() || source != identity.FromEnvironment {
		t.Fatalf("ARC_KEY: %v %q %v", me, source, err)
	}
}

func TestAnEmptySelectorFailsAndNeverFallsThrough(t *testing.T) {
	store, first, _ := testStore(t)
	if err := store.SetDefault(first.Name()); err != nil {
		t.Fatal(err)
	}

	write(t, "arc.key", " \n")
	_, source, err := store.Active()
	if !errors.Is(err, identity.ErrEmpty) || source != identity.FromDirectory {
		t.Fatalf("an empty arc.key gave source %q, err %v", source, err)
	}
	if !strings.Contains(err.Error(), "arc.key") {
		t.Errorf("the error does not name arc.key: %v", err)
	}

	os.Remove("arc.key")
	write(t, store.DefaultFile(), "")
	if _, _, err := store.Active(); !errors.Is(err, identity.ErrEmpty) {
		t.Fatalf("an empty default.key gave %v", err)
	}
}

func TestAnUnknownSelectorNamesTheSelectorAndTheName(t *testing.T) {
	store, _, _ := testStore(t)

	t.Setenv("ARC_KEY", "nobody-here")
	_, _, err := store.Active()
	if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{"ARC_KEY", "nobody-here"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestTheLegacyDefaultAnswersOnlyWithoutDefaultKey(t *testing.T) {
	store, first, second := testStore(t)

	write(t, store.LegacyDefaultFile(), first.Name())
	me, source, err := store.Active()
	if err != nil || me.Name() != first.Name() || source != identity.FromLegacyDefault {
		t.Fatalf("legacy: %v %q %v", me, source, err)
	}

	write(t, store.DefaultFile(), second.Name())
	if me, source, err = store.Active(); err != nil || me.Name() != second.Name() || source != identity.FromDefault {
		t.Fatalf("default.key did not win: %v %q %v", me, source, err)
	}
}

func TestSetDefaultRetiresTheLegacySelector(t *testing.T) {
	store, first, second := testStore(t)
	write(t, store.LegacyDefaultFile(), first.Name())

	if err := store.SetDefault(second.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.LegacyDefaultFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default_key is still there: %v", err)
	}

	me, err := store.Default()
	if err != nil || me.Name() != second.Name() {
		t.Fatalf("default %v, err %v", me, err)
	}
}

func TestRemovingTheLegacyDefaultKeyClearsItsSelector(t *testing.T) {
	store, first, _ := testStore(t)
	write(t, store.LegacyDefaultFile(), first.Name())

	if err := store.Remove(first.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "default_key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default_key is still there: %v", err)
	}
	if _, err := store.Default(); !errors.Is(err, identity.ErrNoDefault) {
		t.Fatalf("err %v", err)
	}
}
