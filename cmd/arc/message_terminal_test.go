package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
	"github.com/gezibash/arc/sdk/storage"
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

// probe is the text of Alfred's probe of ac2ff05: a title change, a clear,
// an override, an invisible character, tags, and variation selectors that
// select nothing. shownProbe is how the rules of docs/clients/SPEC.md,
// section 3.9, show it; it has no backslash, so Escape and EscapeData
// give the same text.
const (
	probe      = "P\x1b]0;pwned\x07\x1b[2J\u202egnp.exe\u200b\U000e0068\U000e0069\U000e0100\ufe0f\ufe0f\ufe0f\u200f(1)\u200fQ"
	shownProbe = `P\x1b]0;pwned\x07\x1b[2J\u202egnp.exe\u200b\U000e0068\U000e0069\U000e0100\ufe0f\ufe0f\ufe0f` + "\u200f(1)\u200fQ"
)

// A live reply is escaped wherever it goes: on a terminal, through a pipe,
// and into a file. Only --raw writes it as it came, and on a terminal it
// says that it does.
func TestALiveReplyIsEscapedWhereverItGoes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	echo := providerBinary(t, "../../adapters/provider/host/testdata/echo")
	provider, providerKey := providerHome(t, relay)
	serveDir(t, ctx, provider, app(t, "echo", fmt.Sprintf("version = 2\n[serve]\ncommand = %q\nmanifest = \"./manifest.json\"\n", echo)), "--interval", "1h")
	caller, _ := providerHome(t, relay)
	ok(t, caller, "", "install", providerKey, "--yes")
	address := "echo+arc://" + providerKey + "/"
	arc := providerBinary(t, ".")
	raw := func(out string) bool { return strings.ContainsAny(out, "\x1b\x07\u202e\u200b\U000e0068\U000e0100") }

	if out := ok(t, caller, "", "call", address, probe); !strings.Contains(out, "ECHO / "+shownProbe) || raw(out) {
		t.Errorf("through a pipe, the reply is %q, want %q", out, "ECHO / "+shownProbe)
	}

	file := filepath.Join(t.TempDir(), "out.txt")
	for _, c := range []struct {
		name string
		args []string
	}{
		{"a terminal", []string{arc, "--home", caller, "call", address, probe}},
		{"| cat", []string{"sh", "-c", `"$0" --home "$1" call "$2" "$3" | cat`, arc, caller, address, probe}},
		{"| tee", []string{"sh", "-c", `"$0" --home "$1" call "$2" "$3" | tee /dev/null`, arc, caller, address, probe}},
		{"> file, then cat", []string{"sh", "-c", `"$0" --home "$1" call "$2" "$3" > "$4"; cat "$4"`, arc, caller, address, probe, file}},
	} {
		tty := startTTY(t, ctx, c.args[0], c.args[1:]...)
		if !waitFor(tty.out, "arc-exit-", 40*time.Second) {
			t.Fatalf("%s: arc call did not end: %q", c.name, tty.out.String())
		}
		if out := tty.out.String(); !strings.Contains(out, "ECHO / "+shownProbe) || raw(out) {
			t.Errorf("%s: the reply is %q, want %q", c.name, out, "ECHO / "+shownProbe)
		}
	}

	tty := startTTY(t, ctx, arc, "--home", caller, "call", "--raw", address, probe)
	if !waitFor(tty.out, "arc-exit-", 40*time.Second) {
		t.Fatalf("--raw: arc call did not end: %q", tty.out.String())
	}
	if out := tty.out.String(); !strings.Contains(out, "ECHO / "+probe) || !strings.Contains(out, "--raw writes the reply as it came") {
		t.Errorf("--raw on a terminal printed %q, want the reply as it came and a warning", out)
	}
}

// The line of an error stays one line, escaped: a refusal cannot forge a
// line of arc, change the terminal, or look like an escape that it is not.
// The cases are those of Alfred's probe of ac2ff05.
func TestAnErrorLineCannotReachTheTerminal(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		{"no\narc: your ring key leaked, run arc keys rotate --force", `arc: the provider refused: no\narc: your ring key leaked, run arc keys rotate --force`},
		{"a\tb\x1b[2J\u202ec\U000e0068\U000e0100\ufe0f\ufe0fd", `arc: the provider refused: a\tb\x1b[2J\u202ec\U000e0068\U000e0100\ufe0f\ufe0fd`},
		{`\x1b[2J fake-escaped`, `arc: the provider refused: \\x1b[2J fake-escaped`},
	} {
		if got := errorLine(fmt.Errorf("the provider refused: %s", c.reason)); got != c.want {
			t.Errorf("errorLine = %q, want %q", got, c.want)
		}
	}
}

// hostileReason is a refusal that tries to forge a line of arc after the
// probe. shownReason is how a line of standard error shows it: escaped, on
// one line.
const (
	hostileReason = "blocked: " + probe + "\narc: forged line"
	shownReason   = "blocked: " + shownProbe + `\narc: forged line`
)

// forged reports a line of out that starts as arc's own lines start.
func forged(out string) bool { return strings.Contains("\n"+out, "\narc: forged") }

// A relay that refuses a message cannot change the terminal or forge a
// line through the warning of arc message send.
func TestARelayRefusalIsEscapedOnOneLine(t *testing.T) {
	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	saved := rl.OnEvent
	rl.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if event.Kind == private.WrapKind {
			return true, hostileReason
		}
		return saved(ctx, event)
	}
	// It closes each subscription with the same reason.
	rl.OnRequest = func(context.Context, nostr.Filter) (bool, string) { return true, hostileReason }
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	alice, _ := providerHome(t, "ws"+strings.TrimPrefix(server.URL, "http"))
	_, bobKey := providerHome(t, "ws"+strings.TrimPrefix(server.URL, "http"))

	var stdout, stderr strings.Builder
	command := root()
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--home", alice, "message", "send", bobKey, "hello"})
	_ = command.Execute()
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, shownReason) || forged(out) || strings.ContainsAny(out, "\x1b\u202e\U000e0068") {
		t.Errorf("arc message send printed %q, want the refusal as %q", out, shownReason)
	}
}

// A relay that answers arc relay ring with a hostile refusal cannot change
// the terminal or forge a line.
func TestARingRefusalIsEscapedOnOneLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/nostr+json" {
			w.Header().Set("Content-Type", "application/nostr+json")
			fmt.Fprint(w, `{"name":"hostile","tags":["arc-rings"]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]string{"error": hostileReason})
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	// The relay list does not reach this relay, which speaks no websocket;
	// the relay is added all the same.
	_, _ = run(t, home, "", "relay", "add", "ws"+strings.TrimPrefix(server.URL, "http"))
	if relays := ok(t, home, "", "relay", "ls"); !strings.Contains(relays, strings.TrimPrefix(server.URL, "http://")) {
		t.Fatalf("the relay was not added: %q", relays)
	}

	for _, words := range [][]string{{"relay", "ring", "show"}, {"relay", "ring", "clear"}} {
		out, _ := run(t, home, "", words...)
		if !strings.Contains(out, shownReason) || forged(out) || strings.ContainsAny(out, "\x1b\u202e\U000e0068") {
			t.Errorf("arc %s printed %q, want the refusal as %q", strings.Join(words, " "), out, shownReason)
		}
	}
}

// The answer of a storage citizen shows escaped, each field on its line.
func TestAStorageAnswerIsEscaped(t *testing.T) {
	account := &storage.Account{Message: hostileReason, Plan: "free\x1b[2J\narc: forged plan", State: "ok\u202e"}
	for _, out := range []string{accountPending("storage-1", account), accountAdmitted("storage-1", account)} {
		if forged(out) || strings.ContainsAny(out, "\x1b\u202e\U000e0068") {
			t.Errorf("the answer printed %q", out)
		}
	}
	if out := accountPending("storage-1", account); !strings.HasPrefix(out, "storage-1: pending: "+shownReason+"\n") {
		t.Errorf("pending printed %q", out)
	}
	if out := accountAdmitted("storage-1", account); !strings.Contains(out, `plan free\x1b[2J\narc: forged plan, ok\u202e`) {
		t.Errorf("admitted printed %q", out)
	}
}
