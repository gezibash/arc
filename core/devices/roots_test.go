package devices_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
)

// TestDataRoots checks the rules of docs/devices/SPEC.md section 3.5: the
// citizen key and each device on the list open the newest root, a revoked
// device gets no wrap of a new generation and cannot open its chain, and the
// newest root opens each older generation.
func TestDataRoots(t *testing.T) {
	ctx := context.Background()
	citizen, laptop, phone := keys.Generate(), keys.Generate(), keys.Generate()

	first, one, err := devices.NewGeneration(ctx, citizen, nil, []nostr.PubKey{laptop.Public, phone.Public}, 0)
	if err != nil || one.Gen != 1 || first.Chain != "" {
		t.Fatalf("the first generation is %d with chain %q, %v; want generation 1 with no chain", one.Gen, first.Chain, err)
	}
	first = roundTrip(t, citizen, first, 1000)
	for _, k := range []keys.Key{citizen, laptop, phone} {
		got, err := first.Unwrap(ctx, k, citizen.Public)
		if err != nil || got != one {
			t.Fatalf("a holder opened %v, %v; want generation 1", got.Gen, err)
		}
	}

	// The phone is revoked: generation 2 is wrapped to the laptop only.
	older, err := first.All(ctx, citizen, citizen.Public)
	if err != nil {
		t.Fatal(err)
	}
	second, two, err := devices.NewGeneration(ctx, citizen, older, []nostr.PubKey{laptop.Public}, first.At)
	if err != nil || two.Gen != 2 || two.Key == one.Key {
		t.Fatalf("the second generation is %d, %v; want a new random root of generation 2", two.Gen, err)
	}
	second = roundTrip(t, citizen, second, 1000)
	if second.At != 1001 {
		t.Fatalf("the second roots event is at %d; want 1001, after the first", second.At)
	}
	if _, err := second.Unwrap(ctx, phone, citizen.Public); err == nil {
		t.Fatal("the revoked phone opened generation 2")
	}
	if _, err := second.Older(one); err == nil {
		t.Fatal("the root of generation 1 opened the chain of generation 2")
	}
	for _, k := range []keys.Key{citizen, laptop} {
		all, err := second.All(ctx, k, citizen.Public)
		if err != nil || len(all) != 2 || all[1] != one.Key || all[2] != two.Key {
			t.Fatalf("a holder of generation 2 got %d roots, %v; want generations 1 and 2", len(all), err)
		}
	}

	// A new device gets the newest root, and through it each older one.
	tablet := keys.Generate()
	granted, err := devices.Grant(ctx, citizen, second, two, []nostr.PubKey{laptop.Public, tablet.Public})
	if err != nil || granted.Devices[laptop.Public] != second.Devices[laptop.Public] {
		t.Fatalf("the grant changed the wrap of the laptop, %v", err)
	}
	if all, err := granted.All(ctx, tablet, citizen.Public); err != nil || all[1] != one.Key || all[2] != two.Key {
		t.Fatalf("the new tablet got %v, %v; want generations 1 and 2", all, err)
	}

	// Data of one app opens only with its own generation and app.
	sealed, err := devices.SealData(two, "journal", "page")
	if err != nil || !strings.HasPrefix(sealed, "2:") {
		t.Fatalf("sealed %q, %v; want generation 2 in clear", sealed, err)
	}
	if text, err := devices.OpenData(two.Key, "journal", sealed); err != nil || text != "page" {
		t.Fatalf("opened %q, %v", text, err)
	}
	if _, err := devices.OpenData(one.Key, "journal", sealed); err == nil {
		t.Fatal("generation 1 opened data of generation 2")
	}
	if _, err := devices.OpenData(two.Key, "files", sealed); err == nil {
		t.Fatal("the files app opened data of the journal")
	}
}

// TestContentKey checks the derivation of base.md 5.2 against a value that
// Python's hmac and hashlib computed for HKDF-SHA256, with no salt, of the
// root 0x01 * 32 and the info "arc-data-v1" || 0x00 || "journal".
func TestContentKey(t *testing.T) {
	var root [32]byte
	for i := range root {
		root[i] = 1
	}
	want, _ := hex.DecodeString("16d0aa9190dd912f448765bd1d9c95c5b06a96c4e66d08b1d303e372c6880690")
	got, err := devices.ContentKey(root, "journal")
	if err != nil || !bytes.Equal(got[:], want) {
		t.Fatalf("content key %x, %v; want %x", got, err, want)
	}
}

// TestRootsEvent checks the event: the kind, the NIP-70 tag, the sealed
// content, and the refusal of another author and of changed content.
func TestRootsEvent(t *testing.T) {
	ctx := context.Background()
	citizen, thief, laptop := keys.Generate(), keys.Generate(), keys.Generate()
	r, root, err := devices.NewGeneration(ctx, citizen, nil, []nostr.PubKey{laptop.Public}, 0)
	if err != nil {
		t.Fatal(err)
	}
	event, err := devices.SealRoots(ctx, citizen, r, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != 10275 || len(event.Tags) != 1 || event.Tags[0][0] != "-" {
		t.Fatalf("the roots event is kind %d with tags %v; want kind 10275 with the NIP-70 tag", event.Kind, event.Tags)
	}
	if strings.Contains(event.Content, laptop.Public.Hex()) || strings.Contains(event.Content, hex.EncodeToString(root.Key[:])) {
		t.Fatal("the roots content is not sealed")
	}
	if _, err := devices.OpenRoots(ctx, laptop, citizen.Public, event); err == nil {
		t.Fatal("a device key opened the roots event without the signer")
	}
	forged, err := devices.SealRoots(ctx, thief, r, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := devices.OpenRoots(ctx, citizen, citizen.Public, forged); err == nil {
		t.Fatal("roots of another author were accepted")
	}
	changed := event
	flip := byte('A')
	if changed.Content[10] == flip {
		flip = 'B'
	}
	changed.Content = changed.Content[:10] + string(flip) + changed.Content[11:]
	if _, err := devices.OpenRoots(ctx, citizen, citizen.Public, changed); err == nil {
		t.Fatal("roots with changed content were accepted")
	}
}

func roundTrip(t *testing.T, citizen keys.Key, r devices.Roots, now nostr.Timestamp) devices.Roots {
	t.Helper()
	event, err := devices.SealRoots(context.Background(), citizen, r, now)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := devices.OpenRoots(context.Background(), citizen, citizen.Public, event)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}
