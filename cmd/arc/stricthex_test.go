package main

import (
	"strings"
	"testing"
)

// A recipient or a contact with a letter that is not hex is refused. On
// amd64 with AVX2, the hex decoder of the Nostr library read Z as f, g as
// 0 and q as a, so mail was queued for a key that nobody typed (Cato's
// test of v0.36.1, 2026-10-19).
func TestAKeyWithALetterThatIsNotHexIsRefused(t *testing.T) {
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	good := "451d098c585bccbcdf68e2a7bf3fa542c2a2503d365c1c079ed100b743855310"
	for _, c := range []struct{ key, want string }{
		{"ZZ" + good[2:], "'Z' at position 1 is not hex"},
		{good[:31] + "g" + good[32:], "'g' at position 32 is not hex"},
		{good[:63] + "q", "'q' at position 64 is not hex"},
		// Above the field prime, so no key has this value.
		{strings.Repeat("f", 64), "it is hex, but no key has this value"},
		{good[:63], "this has 63"},
	} {
		if _, err := run(t, home, "", "message", "send", c.key, "hello"); err == nil || !strings.Contains(err.Error(), "the recipient is not a key: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("arc message send %s… gave %v, want %q", c.key[:6], err, c.want)
		}
		if _, err := run(t, home, "", "contacts", "add", c.key); err == nil || !strings.Contains(err.Error(), "the contact is not a key: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("arc contacts add %s… gave %v, want %q", c.key[:6], err, c.want)
		}
	}
	if out := ok(t, home, "", "message", "outbox"); !strings.Contains(out, "no messages sent") {
		t.Errorf("the outbox holds a message after the refusals:\n%s", out)
	}
}
