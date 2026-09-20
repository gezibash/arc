package lists_test

import (
	"path/filepath"
	"testing"

	"github.com/gezibash/arc/lists"
)

func TestAListHoldsItsPeers(t *testing.T) {
	store := &lists.Store{Dir: filepath.Join(t.TempDir(), "arc")}

	if members := store.Members("dm", "friends"); members != nil {
		t.Errorf("a list that is not there holds %v", members)
	}

	members, err := store.Add("dm", "friends", []string{"ada", "grace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("the list holds %v", members)
	}

	// A peer that stands already is not added twice.
	members, err = store.Add("dm", "friends", []string{"ada", "alan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Errorf("the list holds %v", members)
	}

	members, err = store.Remove("dm", "friends", []string{"grace"})
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0] != "ada" {
		t.Errorf("the list holds %v", members)
	}

	if names := store.Names("dm"); len(names) != 1 || names[0] != "friends" {
		t.Errorf("the tool holds %v", names)
	}

	if _, err := store.Remove("dm", "friends", nil); err != nil {
		t.Fatal(err)
	}
	if members := store.Members("dm", "friends"); members != nil {
		t.Errorf("the list stands after the removal: %v", members)
	}
}

func TestANameMustHold(t *testing.T) {
	store := &lists.Store{Dir: filepath.Join(t.TempDir(), "arc")}

	for _, name := range []string{"", "-bad", "../escape", "Upper"} {
		if _, err := store.Add("dm", name, []string{"ada"}); err != lists.ErrInvalidName {
			t.Errorf("the name %q gave %v", name, err)
		}
	}
	if _, err := store.Add("../escape", "friends", []string{"ada"}); err != lists.ErrInvalidName {
		t.Error("a tool name that climbs passed")
	}
}
