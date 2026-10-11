package server

import (
	"errors"
	"testing"
	"time"
)

// TestGrantBindsAccount refuses a second owner that uses the pseudonym of
// another owner. The pseudonym alone is not a credential.
func TestGrantBindsAccount(t *testing.T) {
	s := &store{dir: t.TempDir(), quota: DefaultQuota, now: time.Now}
	const account = "AAAAAAAAAAAAAAAAAAAAAA"
	first, second := "aa", "bb"
	if err := s.authorize(account, first); err != nil {
		t.Fatal(err)
	}
	if err := s.authorize(account, first); err != nil {
		t.Fatalf("the first owner opens its account again: %v", err)
	}
	if err := s.authorize(account, second); !errors.Is(err, errNoGrant) {
		t.Fatalf("a second key in the account gave %v, want errNoGrant", err)
	}
}
