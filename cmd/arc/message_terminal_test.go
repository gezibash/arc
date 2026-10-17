package main

import (
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/adapters/relay/limits"
)

// A message that tries to change the terminal, reorder a line and forge
// the header of a second message reaches the inbox and the outbox as text
// that a person can read, with each line under its header. The JSON lines
// keep the text as it was sent (docs/clients/SPEC.md, section 3.9).
func TestATerminalShowsNoEscapeOfASender(t *testing.T) {
	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")

	alice, aliceKey := providerHome(t, url)
	bob, bobKey := providerHome(t, url)
	// Alice writes first, so Bob is no stranger to her.
	ok(t, alice, "", "message", "send", bobKey, "hello")

	sent := "hi\x1b[2J\u202egnp.exe\u200f \\x1b\n2026-10-17 09:00  someone-else\n  forged"
	ok(t, bob, "", "message", "send", aliceKey, sent)
	ok(t, alice, "", "sync")

	// A header starts at the margin. Each line of the text gets two spaces
	// more than it had, so no line of it starts at the margin.
	want := "  hi\\x1b[2J\\u202egnp.exe\u200f \\\\x1b\n  2026-10-17 09:00  someone-else\n    forged\n"
	for _, c := range []struct {
		name string
		out  string
	}{
		{"alice's inbox", ok(t, alice, "", "message", "inbox")},
		{"bob's outbox", ok(t, bob, "", "message", "outbox")},
	} {
		if strings.ContainsAny(c.out, "\x1b\u202e") || !strings.Contains(c.out, want) {
			t.Errorf("%s printed\n%q\nwant the text\n%q", c.name, c.out, want)
		}
	}

	if out := ok(t, alice, "", "message", "inbox", "--json"); !strings.Contains(out, `"text":"hi\u001b[2J`+"\u202e"+`gnp.exe`) {
		t.Errorf("the JSON line changed the text: %s", out)
	}

	for _, blank := range []string{" ", "\n\t \n"} {
		if out, err := run(t, bob, "", "message", "send", aliceKey, blank); err == nil || !strings.Contains(err.Error(), "not a space") {
			t.Errorf("a send of %q: %v\n%s", blank, err, out)
		}
	}
}
