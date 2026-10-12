package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	wire "github.com/gezibash/arc/sdk/storage"
)

// TestGrantSets checks how the app finds the account of a caller. An account
// key uses its own account. A device key uses the account whose newest grant
// set holds it. A newer set without the device revokes it, and an older set
// cannot bring it back.
func TestGrantSets(t *testing.T) {
	account, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	phone, stranger := strings.Repeat("c", 64), strings.Repeat("d", 64)
	s := &server{store: &store{dir: t.TempDir(), owners: []string{account, other}, quota: DefaultQuota, now: time.Now}}

	if got, err := s.accountOf(account); err != nil || got != account {
		t.Fatalf("the account key got %q, %v; want its own account", got, err)
	}
	if _, err := s.accountOf(phone); !errors.Is(err, errNoGrant) {
		t.Fatalf("a device with no grant got %v, want errNoGrant", err)
	}
	if err := s.store.grant(account, []string{phone}, 100); err != nil {
		t.Fatal(err)
	}
	if got, err := s.accountOf(phone); err != nil || got != account {
		t.Fatalf("a granted device got %q, %v; want the account that granted it", got, err)
	}
	if _, err := s.accountOf(stranger); !errors.Is(err, errNoGrant) {
		t.Fatalf("a key that no set holds got %v, want errNoGrant", err)
	}

	if err := s.store.grant(account, nil, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := s.accountOf(phone); !errors.Is(err, errNoGrant) {
		t.Fatalf("a revoked device got %v, want errNoGrant", err)
	}
	err := s.store.grant(account, []string{phone}, 100)
	if r, ok := errors.AsType[refusal](err); !ok || r.code != wire.ErrStaleGrants {
		t.Fatalf("an older grant set gave %v, want %s", err, wire.ErrStaleGrants)
	}
	if _, err := s.accountOf(phone); !errors.Is(err, errNoGrant) {
		t.Fatalf("an older grant set brought back a revoked device: %v", err)
	}
	if err := s.store.grant(account, nil, 101); err != nil {
		t.Fatalf("the same set sent again gave %v, want nil", err)
	}
}
