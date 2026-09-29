package store_test

import (
	"testing"

	"fiatjaf.com/nostr"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
)

func TestUnavailableStoreIsNotEmpty(t *testing.T) {
	s, err := boltstore.Open(t.TempDir())
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
