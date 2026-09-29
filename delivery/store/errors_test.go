package store_test

import (
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/store"
)

func TestUnavailableStoreIsNotEmpty(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.Query(nostr.Filter{}); err == nil {
		t.Fatal("unavailable database appeared empty")
	}
	if _, err := s.Has(nostr.ID{}); err == nil {
		t.Fatal("unavailable database reported missing data")
	}
}
