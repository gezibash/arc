package citizen

import (
	"context"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
)

// countingSigner is the citizen key on a signer, and counts each request
// that opens or seals.
type countingSigner struct {
	keys.Key
	calls *int
}

func (c countingSigner) Encrypt(ctx context.Context, text string, to nostr.PubKey) (string, error) {
	*c.calls++
	return c.Key.Encrypt(ctx, text, to)
}

func (c countingSigner) Decrypt(ctx context.Context, text string, from nostr.PubKey) (string, error) {
	*c.calls++
	return c.Key.Decrypt(ctx, text, from)
}

// TestTheCachesOpenWithNoSigner reopens a session of the machine that holds
// the citizen key: the device list on its disk opens with the machine key,
// so the session asks the signer nothing. A cache that an older arc sealed
// with the citizen key still opens, through the signer.
func TestTheCachesOpenWithNoSigner(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	citizen, machine := keys.Generate(), keys.Generate()
	device := keys.Generate().Public
	open := func(cache keys.Key) (*Session, *int) {
		calls := 0
		signer := countingSigner{Key: citizen, calls: &calls}
		sess, err := Open(Config{Home: home, Root: home, Key: keys.Key{Public: citizen.Public}, Signer: signer, CacheKey: cache})
		if err != nil {
			t.Fatal(err)
		}
		return sess, &calls
	}

	sess, _ := open(machine)
	if err := sess.keepList(ctx, devices.List{Devices: []devices.Device{{Key: device, Name: "phone"}}, At: 1}, true); err != nil {
		t.Fatal(err)
	}
	sess.Close()

	sess, calls := open(machine)
	if !sess.signers.list().Has(device) {
		t.Fatal("the device list on the disk did not open")
	}
	if *calls != 0 {
		t.Fatalf("opening the session asked the signer %d times", *calls)
	}
	sess.Close()

	// An older arc sealed the cache with the citizen key.
	sess, _ = open(keys.Key{})
	if err := sess.keepList(ctx, devices.List{Devices: []devices.Device{{Key: device, Name: "phone"}}, At: 2}, true); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	sess, calls = open(machine)
	defer sess.Close()
	if !sess.signers.list().Has(device) || *calls == 0 {
		t.Fatalf("the cache of an older arc did not open through the signer (%d requests)", *calls)
	}
}
