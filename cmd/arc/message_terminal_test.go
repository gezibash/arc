package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/internal/testrelay"
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

	for _, blank := range []string{" ", "\n\t \n", "\u200b\u2060", "\U000e0067\U000e007f"} {
		if out, err := run(t, bob, "", "message", "send", aliceKey, blank); err == nil || !strings.Contains(err.Error(), "a person can see") {
			t.Errorf("a send of %q: %v\n%s", blank, err, out)
		}
	}
}

// hostile is a text that hides a word in tag characters, breaks a line with
// U+2028, and colours the terminal. shownHostile is how a terminal must show
// it (docs/clients/SPEC.md, section 3.9).
const (
	hostile      = "red\x1b[31m line\u2028two\U000e0068\U000e0069"
	shownHostile = `red\x1b[31m line\u2028two\U000e0068\U000e0069`
)

// Held mail from a stranger, and a message that arc message watch prints,
// reach the terminal escaped.
func TestHeldAndWatchedTextIsEscaped(t *testing.T) {
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

	// A stranger's message, without proof of work, is held.
	aliceKeyValue, err := nostr.PubKeyFromHex(aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	stranger := keys.Generate()
	rumor := private.Rumor(stranger, 14, "held "+hostile, nostr.Tags{{"p", aliceKey}}, time.Now())
	wrap, err := private.Wrap(context.Background(), stranger, aliceKeyValue, rumor, private.RelayForm, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := (relay.Relay{URL: url, Signer: stranger}).Send(context.Background(), wrap); err != nil {
		t.Fatal(err)
	}
	ok(t, alice, "", "sync")
	if out := ok(t, alice, "", "message", "held"); strings.ContainsAny(out, "\x1b\u2028\U000e0068") || !strings.Contains(out, "  held "+shownHostile+"\n") {
		t.Errorf("arc message held printed %q, want the text %q", out, shownHostile)
	}

	// A message from a contact, that a watch prints.
	ok(t, alice, "", "message", "send", bobKey, "hello")
	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	ok(t, bob, "", "message", "send", aliceKey, "watched "+hostile)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out lockedBuilder
	done := make(chan error, 1)
	go func() {
		command := root()
		command.SetOut(&out)
		command.SetErr(&strings.Builder{})
		command.SetArgs([]string{"--home", alice, "message", "watch", "--since", since})
		done <- command.ExecuteContext(ctx)
	}()
	printed := func() bool { return strings.Contains(out.String(), "  watched "+shownHostile+"\n") }
	if !within(20*time.Second, printed) {
		t.Errorf("arc message watch printed %q, want the text %q", out.String(), shownHostile)
	}
	cancel()
	<-done
	if strings.ContainsAny(out.String(), "\x1b\u2028\U000e0068") {
		t.Errorf("arc message watch printed a raw character: %q", out.String())
	}
}

// lockedBuilder is a strings.Builder that a command writes while a test
// reads it.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A live reply goes as it came through a pipe, so a program gets its bytes,
// and shows escaped on a terminal (docs/clients/SPEC.md, section 3.9).
func TestALiveReplyIsEscapedOnATerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	echo := providerBinary(t, "../../adapters/provider/host/testdata/echo")
	provider, providerKey := providerHome(t, relay)
	serveDir(t, ctx, provider, app(t, "echo", fmt.Sprintf("version = 2\n[serve]\ncommand = %q\nmanifest = \"./manifest.json\"\n", echo)), "--interval", "1h")
	caller, _ := providerHome(t, relay)
	ok(t, caller, "", "install", providerKey, "--yes")
	address := "echo+arc://" + providerKey + "/"

	if out := ok(t, caller, "", "call", address, "piped "+hostile); !strings.Contains(out, "piped "+hostile) {
		t.Errorf("through a pipe, the reply is %q, want the text as it came", out)
	}

	tty := startTTY(t, ctx, providerBinary(t, "."), "--home", caller, "call", address, "shown "+hostile)
	if !waitFor(tty.out, "arc-exit-", 30*time.Second) {
		t.Fatalf("arc call did not end: %q", tty.out.String())
	}
	if out := tty.out.String(); !strings.Contains(out, "shown "+shownHostile) || strings.ContainsAny(out, "\x1b\u2028\U000e0068") {
		t.Errorf("on a terminal, the reply is %q, want %q", out, "shown "+shownHostile)
	}
}

// The line of an error loses what could change the terminal or hide text,
// and keeps a value that arc quoted as it was quoted.
func TestAnErrorLineCannotReachTheTerminal(t *testing.T) {
	err := fmt.Errorf("the provider refused: %s; the scheme %q", "no\x1b[2J way\u200b\U000e0068", "x\x1b")
	if got, want := errorLine(err), `arc: the provider refused: no[2J way; the scheme "x\x1b"`; got != want {
		t.Errorf("errorLine = %q, want %q", got, want)
	}
}
