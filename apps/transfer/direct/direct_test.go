package direct

import "testing"

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
