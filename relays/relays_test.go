package relays_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gezibash/arc/relays"
)

func pin(n int) string { return strings.Repeat(fmt.Sprintf("%02x", n), 32) }

// Joins at the same time each keep their relay: the lock stops one write from
// replacing another.
func TestRememberKeepsEveryRelayOfJoinsAtTheSameTime(t *testing.T) {
	store := &relays.Store{Dir: t.TempDir()}

	var group sync.WaitGroup
	for n := 1; n <= 16; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := store.Remember(fmt.Sprintf("relay-%d.example:7331", n), pin(n)); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()

	document, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Relays) != 16 {
		t.Fatalf("the store holds %d relays, not 16", len(document.Relays))
	}
}

func TestRememberNeverChangesAPin(t *testing.T) {
	store := &relays.Store{Dir: t.TempDir()}

	if err := store.Remember("relay.example:7331", pin(1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Remember("relay.example:7331", pin(2)); !errors.Is(err, relays.ErrPinMismatch) {
		t.Fatalf("err %v", err)
	}
}
