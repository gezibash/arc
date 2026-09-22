package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip46"
)

func TestTheBunkerPolicy(t *testing.T) {
	owner, other := nostr.Generate().Public(), nostr.Generate().Public()
	sign := func(kind int) nip46.Request {
		event := nostr.Event{Kind: nostr.Kind(kind), CreatedAt: 1, Tags: nostr.Tags{}}
		body, _ := event.MarshalJSON()
		return nip46.Request{Method: "sign_event", Params: []string{string(body)}}
	}
	decrypt := func(from nostr.PubKey) nip46.Request {
		return nip46.Request{Method: "nip44_decrypt", Params: []string{from.Hex(), "ciphertext"}}
	}

	cases := []struct {
		name   string
		policy policy
		req    nip46.Request
		want   string
	}{
		{"an allowed kind", policy{kinds: []nostr.Kind{11}, decrypt: "self"}, sign(11), ""},
		{"a kind not allowed", policy{kinds: []nostr.Kind{11}, decrypt: "self"}, sign(1111), "does not sign kind 1111"},
		{"authentication", policy{kinds: []nostr.Kind{11}, decrypt: "self"}, sign(22242), ""},
		{"any kind", policy{decrypt: "self"}, sign(0), ""},
		{"self opens own", policy{decrypt: "self"}, decrypt(owner), ""},
		{"self refuses others", policy{decrypt: "self"}, decrypt(other), "decrypts only"},
		{"none refuses own", policy{decrypt: "none"}, decrypt(owner), "does not decrypt"},
		{"all opens others", policy{decrypt: "all"}, decrypt(other), ""},
		{"nip04 too", policy{decrypt: "self"}, nip46.Request{Method: "nip04_decrypt", Params: []string{other.Hex(), "x"}}, "decrypts only"},
		{"encrypt is open", policy{decrypt: "none"}, nip46.Request{Method: "nip44_encrypt", Params: []string{other.Hex(), "x"}}, ""},
	}
	for _, c := range cases {
		err := c.policy.refusal(c.req, owner)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

// run runs arcn in a home, with its standard output captured.
func run(t *testing.T, home string, stdin string, args ...string) (string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = write
	command := root()
	command.SetIn(strings.NewReader(stdin))
	command.SetArgs(append([]string{"--home", home}, args...))
	runErr := command.Execute()
	os.Stdout = saved
	write.Close()
	out, _ := io.ReadAll(read)
	return string(out), runErr
}

// ok runs arcn, and fails the test when the command fails.
func ok(t *testing.T, home string, stdin string, args ...string) string {
	t.Helper()
	out, err := run(t, home, stdin, args...)
	if err != nil {
		t.Fatalf("arcn %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// Two identities in one home keep apart what belongs to each key: here, the
// relays. The first identity is the default; --key picks the other.
func TestTwoIdentitiesKeepTheirOwnRelays(t *testing.T) {
	home := t.TempDir()
	first := strings.Fields(ok(t, home, "", "keys", "gen"))[0]
	second := strings.Fields(ok(t, home, "", "keys", "gen"))[0]

	who := ok(t, home, "", "whoami")
	if !strings.HasPrefix(who, first+"\n") || !strings.Contains(who, "chosen by the default") {
		t.Fatalf("whoami gave %q, want %s chosen by the default", who, first)
	}

	ok(t, home, "", "relay", "add", "ws://127.0.0.1:1")
	if out := ok(t, home, "", "--key", second, "relay", "ls"); strings.Contains(out, "127.0.0.1:1") {
		t.Fatalf("%s sees the relay of %s: %q", second, first, out)
	}

	ok(t, home, "", "keys", "use", second)
	if out := ok(t, home, "", "keys", "list"); !strings.Contains(out, "* "+second) {
		t.Fatalf("keys list does not mark %s as the default: %q", second, out)
	}
}

// The name of an identity cannot reach outside the directory of identities.
func TestAKeyNameCannotLeaveItsDirectory(t *testing.T) {
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside", "..", "nobody-here"} {
		if _, err := run(t, home, "", "--key", name, "whoami"); err == nil {
			t.Errorf("--key %q was taken", name)
		}
		if _, err := run(t, home, "", "keys", "use", name); err == nil {
			t.Errorf("keys use %q was taken", name)
		}
	}
}

// A key that does not parse leaves nothing behind, and the same key twice is
// refused.
func TestKeysAddRefusesABadKeyAndADuplicate(t *testing.T) {
	home := t.TempDir()
	if _, err := run(t, home, "not a key", "keys", "add"); err == nil {
		t.Fatal("keys add took a line that is not a key")
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "citizens")); len(entries) != 0 {
		t.Fatalf("a refused key left %d entries behind", len(entries))
	}

	secret := nostr.Generate()
	if _, err := run(t, home, secret.Hex(), "keys", "add"); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, home, secret.Hex(), "keys", "add")
	if err == nil || !strings.Contains(err.Error(), "already on this machine") {
		t.Fatalf("the same key twice gave %v", err)
	}
}

// Removing the default identity leaves no default, so the next identity
// that this machine makes becomes the default.
func TestRemovingTheDefaultLetsTheNextOneTakeItsPlace(t *testing.T) {
	home := t.TempDir()
	gone := strings.Fields(ok(t, home, "", "keys", "gen"))[0]
	ok(t, home, "", "keys", "remove", gone, "--yes")
	if _, err := os.Stat(filepath.Join(home, "citizens", gone)); !os.IsNotExist(err) {
		t.Fatalf("the directory of %s is still there: %v", gone, err)
	}

	next := strings.Fields(ok(t, home, "", "keys", "gen"))[0]
	who, err := run(t, home, "", "whoami")
	if err != nil || !strings.HasPrefix(who, next+"\n") {
		t.Fatalf("whoami gave %q, %v; want %s", who, err, next)
	}
}

// tool remove takes out the install that a name runs, and leaves the others.
func TestToolRemoveTakesOutOneInstall(t *testing.T) {
	home := t.TempDir()
	name := strings.Fields(ok(t, home, "", "keys", "gen"))[0]
	a, b := nostr.Generate().Public().Hex(), nostr.Generate().Public().Hex()
	list := `[{"provider":"` + a + `","id":"journal","name":"x","as":"journal"},` +
		`{"provider":"` + b + `","id":"exec","name":"y"}]`
	if err := os.WriteFile(filepath.Join(home, "citizens", name, "installs.json"), []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}

	ok(t, home, "", "tool", "remove", "journal")
	out := ok(t, home, "", "tool", "list")
	if strings.Contains(out, "journal") || !strings.Contains(out, "arcn call y") {
		t.Fatalf("tool list after removing journal gave %q", out)
	}
	if _, err := run(t, home, "", "tool", "remove", "journal"); err == nil {
		t.Fatal("removing journal twice succeeded")
	}
}

// A capability command takes --key before its name.
func TestDispatchReadsKey(t *testing.T) {
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	_, err := run(t, home, "", "--key", "nobody-here", "journal", "read", "x")
	if err == nil || !strings.Contains(err.Error(), `no identity "nobody-here"`) {
		t.Fatalf("dispatch with --key gave %v", err)
	}
}
