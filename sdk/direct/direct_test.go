package direct

import (
	"slices"
	"testing"
)

// An end with no STUN setting asks its first relay, on port 3478. A
// setting overrides the relay, and "none" gives no server.
func TestSTUNURL(t *testing.T) {
	relays := []string{"wss://relay.example.org/path", "ws://second.example.org:7447"}
	for _, c := range []struct {
		value  string
		relays []string
		want   string
	}{
		{"", relays, "stun:relay.example.org:3478"},
		{"", []string{"ws://127.0.0.1:7447"}, "stun:127.0.0.1:3478"},
		{"", []string{"ws://[2001:db8::1]:7447"}, "stun:[2001:db8::1]:3478"},
		{"", nil, ""},
		{"", []string{"http://relay.example.org"}, ""},
		{"none", relays, ""},
		{"stun:stun.example.net:19302", relays, "stun:stun.example.net:19302"},
	} {
		if got := STUNURL(c.value, c.relays); got != c.want {
			t.Errorf("STUNURL(%q, %q) = %q, want %q", c.value, c.relays, got, c.want)
		}
	}
}

// TestSTUNURLs gives the STUN server of each relay, in the order of the
// relays (docs/direct/SPEC.md, section 3.1).
func TestSTUNURLs(t *testing.T) {
	relays := []string{"wss://relay.example.org/path", "http://not.a.relay", "ws://second.example.org:7447", "wss://relay.example.org/other"}
	got := STUNURLs("", relays)
	want := []string{"stun:relay.example.org:3478", "stun:second.example.org:3478"}
	if !slices.Equal(got, want) {
		t.Errorf("STUNURLs of %q = %q, want %q", relays, got, want)
	}
	if got := STUNURLs("none", relays); got != nil {
		t.Errorf("STUNURLs(none) = %q, want none", got)
	}
	if got := STUNURLs("stun:stun.example.net:19302", relays); !slices.Equal(got, []string{"stun:stun.example.net:19302"}) {
		t.Errorf("a set STUN server gave %q", got)
	}
	t.Setenv("ARC_STUN", "")
	if o := EnvOptions(relays); o.STUN != want[0] || !slices.Equal(o.MoreSTUN, want[1:]) {
		t.Errorf("EnvOptions: STUN %q and MoreSTUN %q, want %q and %q", o.STUN, o.MoreSTUN, want[0], want[1:])
	}
}
