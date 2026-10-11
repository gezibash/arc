package devices_test

import (
	"context"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
)

// TestDeviceList checks the rules of docs/devices/SPEC.md section 3: only
// the citizen opens the list, a list from another key or with a changed
// content is refused, and each new list replaces the one before it.
func TestDeviceList(t *testing.T) {
	ctx := context.Background()
	citizen, thief := keys.Generate(), keys.Generate()
	phone := keys.Generate().Public

	l, err := devices.List{}.Add(devices.Device{Key: phone, Name: "phone", Added: 100})
	if err != nil {
		t.Fatal(err)
	}
	event, err := devices.Seal(ctx, citizen, l, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != 10274 || event.CreatedAt != 1000 || len(event.Tags) != 1 || event.Tags[0][0] != "-" {
		t.Fatalf("the list event is kind %d at %d with tags %v; want kind 10274 at 1000 with the NIP-70 tag", event.Kind, event.CreatedAt, event.Tags)
	}
	if strings.Contains(event.Content, "phone") || strings.Contains(event.Content, phone.Hex()) {
		t.Fatal("the list content is not sealed")
	}

	opened, err := devices.Open(ctx, citizen, citizen.Public, event)
	if err != nil || !opened.Has(phone) || opened.At != 1000 {
		t.Fatalf("the citizen opened %+v, %v; want the phone at 1000", opened, err)
	}

	// Another key signs a copy of the sealed list of the citizen.
	forged := nostr.Event{Kind: event.Kind, CreatedAt: 2000, Content: event.Content, Tags: event.Tags}
	if err := thief.SignEvent(ctx, &forged); err != nil {
		t.Fatal(err)
	}
	if _, err := devices.Open(ctx, citizen, citizen.Public, forged); err == nil {
		t.Fatal("a list that another key signed was opened")
	}
	other, err := devices.Seal(ctx, thief, l, 2000)
	if err != nil {
		t.Fatal(err)
	}
	changed := event
	changed.Content = other.Content
	if _, err := devices.Open(ctx, citizen, citizen.Public, changed); err == nil {
		t.Fatal("a list with changed content was opened")
	}

	// A revocation in the same second must still replace the list.
	revoked, err := devices.Seal(ctx, citizen, opened.Remove(phone), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.CreatedAt != 1001 || !devices.Newer(revoked, event) {
		t.Fatalf("the revocation is at %d; want 1001, later than the list before it", revoked.CreatedAt)
	}

	if _, err := opened.Add(devices.Device{Key: phone, Name: "other"}); err == nil {
		t.Fatal("the same device key was added twice")
	}
	if _, err := opened.Add(devices.Device{Key: keys.Generate().Public, Name: "phone"}); err == nil {
		t.Fatal("two devices got the same name")
	}
	if _, err := opened.Add(devices.Device{Key: nostr.PubKey{1}, Name: "bad\nname"}); err == nil {
		t.Fatal("a name with a newline was accepted")
	}
}
