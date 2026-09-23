package relaylist_test

import (
	"slices"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/relaylist"
)

// The list of the NIP-65 text: a relay without a marker is for reading and
// writing, a "read" relay only for reading, and a "write" relay only for
// writing.
func TestReadFollowsTheMarkersOfNIP65(t *testing.T) {
	list := nostr.Event{Kind: relaylist.Kind, Tags: nostr.Tags{
		{"r", "wss://alicerelay.example.com"},
		{"r", "wss://brando-relay.com"},
		{"r", "wss://expensive-relay.example2.com", "write"},
		{"r", "wss://nostr-relay.example.com", "read"},
		{"p", "wss://not-a-relay.example.com"},
	}}

	read, write := relaylist.Read(list)

	wantRead := []string{"wss://alicerelay.example.com", "wss://brando-relay.com", "wss://nostr-relay.example.com"}
	wantWrite := []string{"wss://alicerelay.example.com", "wss://brando-relay.com", "wss://expensive-relay.example2.com"}
	if !slices.Equal(read, wantRead) {
		t.Errorf("read relays %v, want %v", read, wantRead)
	}
	if !slices.Equal(write, wantWrite) {
		t.Errorf("write relays %v, want %v", write, wantWrite)
	}
}
