package private_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/private"
)

func TestIdentityAdapterPreservesCryptoCancellation(t *testing.T) {
	a, b := keys.Generate(), keys.Generate()
	identity := keys.Identity{Public: a.Public, Keyer: a}
	rumor := private.Rumor(a, 14, "message", nil, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := private.Seal(ctx, identity, b.Public, rumor); !errors.Is(err, context.Canceled) {
		t.Fatalf("seal=%v", err)
	}
	seal, err := private.Seal(context.Background(), a, b.Public, rumor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := private.OpenSeal(ctx, keys.Identity{Public: b.Public, Keyer: b}, seal); !errors.Is(err, context.Canceled) {
		t.Fatalf("open=%v", err)
	}
}
