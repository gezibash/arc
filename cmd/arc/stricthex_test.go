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
	for _, bad := range []string{"ZZ" + good[2:], good[:31] + "g" + good[32:], good[:63] + "q"} {
		if _, err := run(t, home, "", "message", "send", bad, "hello"); err == nil || !strings.Contains(err.Error(), "a recipient is 64 characters of hex") {
			t.Errorf("arc message send %s… gave %v, want the recipient refused", bad[:6], err)
		}
		if _, err := run(t, home, "", "contacts", "add", bad); err == nil || !strings.Contains(err.Error(), "a contact is 64 characters of hex") {
			t.Errorf("arc contacts add %s… gave %v, want the contact refused", bad[:6], err)
		}
	}
	if out := ok(t, home, "", "message", "outbox"); !strings.Contains(out, "no messages sent") {
		t.Errorf("the outbox holds a message after the refusals:\n%s", out)
	}
}
