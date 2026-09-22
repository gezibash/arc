package main

import (
	"testing"

	"github.com/gezibash/arc/identity"
)

func emptyStore(t *testing.T) *identity.Store {
	t.Helper()
	t.Setenv("ARC_LEGACY_KEY", "")
	t.Chdir(t.TempDir())
	return &identity.Store{Dir: t.TempDir()}
}

// --generate makes the first key, and every later start finds the same key.
func TestGenerateMakesOneKeyAndKeepsIt(t *testing.T) {
	keys := emptyStore(t)

	first, err := relayIdentity(keys, "", true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := relayIdentity(keys, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name() != again.Name() {
		t.Fatalf("the second start made %s, after %s", again.Name(), first.Name())
	}

	held, err := keys.List()
	if err != nil || len(held) != 1 {
		t.Fatalf("the store holds %d keys, err %v", len(held), err)
	}
}

// A key takes its name from its public key, so --generate cannot make a key
// of the name that --key asks for.
func TestGenerateRefusesANameItCannotGive(t *testing.T) {
	keys := emptyStore(t)

	if _, err := relayIdentity(keys, "my-relay", true); err == nil {
		t.Fatal("--key my-relay --generate passed")
	}
	if held, _ := keys.List(); len(held) != 0 {
		t.Fatalf("the refused start made %d keys", len(held))
	}
}

func TestKeyNamesAKeyOfTheStore(t *testing.T) {
	keys := emptyStore(t)
	made, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}

	me, err := relayIdentity(keys, made.Name(), true)
	if err != nil || me.Name() != made.Name() {
		t.Fatalf("got %v, err %v", me, err)
	}
}

// Keys without a default are a choice for the operator, and never a reason to
// make another key.
func TestGenerateRefusesAStoreWithKeysAndNoDefault(t *testing.T) {
	keys := emptyStore(t)
	if _, err := keys.Generate(); err != nil {
		t.Fatal(err)
	}

	if _, err := relayIdentity(keys, "", true); err == nil {
		t.Fatal("a store with a key and no default made another key")
	}
	if held, _ := keys.List(); len(held) != 1 {
		t.Fatalf("the store holds %d keys", len(held))
	}
}
