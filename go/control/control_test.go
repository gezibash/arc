package control_test

import (
	"path/filepath"
	"testing"

	"github.com/gezibash/arc/go/control"
	"github.com/gezibash/arc/go/identity"
)

func testPlane(t *testing.T) (*control.Store, *identity.Identity) {
	t.Helper()

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return &control.Store{Dir: filepath.Join(t.TempDir(), "arc")}, me
}

func TestPublishAndResolve(t *testing.T) {
	plane, me := testPlane(t)

	entry, err := plane.Publish(me)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != me.Name() || entry.Status != control.Active {
		t.Errorf("entry = %+v", entry)
	}

	for _, query := range []string{me.Name(), me.ShortName(), me.EncodePublicKey()[:8]} {
		found, err := plane.Resolve(query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if len(found) != 1 || found[0].PublicKey != me.EncodePublicKey() {
			t.Errorf("%s found %v", query, found)
		}
	}

	if _, err := plane.Resolve("nobody"); err != control.ErrNotFound {
		t.Errorf("a name that answers to no one gave %v", err)
	}

	held, err := plane.Get(me.PublicKey)
	if err != nil || held.Name != me.Name() {
		t.Errorf("get = %+v, %v", held, err)
	}
}

func TestRevokeStopsAnAnswer(t *testing.T) {
	plane, me := testPlane(t)

	if _, err := plane.Publish(me); err != nil {
		t.Fatal(err)
	}
	if err := plane.Revoke(me.PublicKey); err != nil {
		t.Fatal(err)
	}

	if _, err := plane.Resolve(me.Name()); err != control.ErrNotFound {
		t.Errorf("a revoked identity answers: %v", err)
	}

	held, err := plane.Get(me.PublicKey)
	if err != nil || held.Status != control.Revoked {
		t.Errorf("entry = %+v, %v", held, err)
	}
}

func TestListsEveryIdentity(t *testing.T) {
	plane, first := testPlane(t)
	second, _ := identity.Generate()

	plane.Publish(first)
	plane.Publish(second)

	entries, err := plane.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the plane holds %d entries", len(entries))
	}
	if entries[0].Name > entries[1].Name {
		t.Error("the entries are out of order")
	}
}
