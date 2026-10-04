package proof

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMessageWatch proves the inbox that a harness reads: arc message watch,
// inbox --json and send --json, through a relay and the normal CLI. The JSON
// line is a contract with the arc-harness repository: a change to it is a
// breaking change.
func TestMessageWatch(t *testing.T) {
	work := t.TempDir()
	limit := 10 * time.Second
	want := func(ok bool, format string, args ...any) {
		t.Helper()
		if !ok {
			t.Fatalf(format, args...)
		}
	}

	url := startRelay(t, filepath.Join(work, "relay"), "--wrap-auth")
	alice := arc(t, filepath.Join(work, "alice"))
	bob := arc(t, filepath.Join(work, "bob"))
	carol := arc(t, filepath.Join(work, "carol"))
	for _, m := range []machine{alice, bob, carol} {
		m.run("keys", "gen")
		m.run("relay", "add", url)
	}
	aliceKey, bobKey := alice.key(), bob.key()

	// send sends one message to bob, and returns the id that send --json
	// prints.
	send := func(from machine, text string) string {
		t.Helper()
		out := from.run("message", "send", "--json", bobKey, text)
		var sent map[string]string
		want(strings.Count(out, "\n") == 1 && json.Unmarshal([]byte(out), &sent) == nil, "send --json printed %q", out)
		want(len(sent) == 3 && len(sent["id"]) == 64 && sent["to"] == bobKey && sent["state"] == "pending",
			"send --json printed %q", out)
		return sent["id"]
	}
	// lines parses JSON lines. Each line holds exactly the five fields.
	lines := func(text string) []map[string]string {
		t.Helper()
		var parsed []map[string]string
		for _, raw := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if raw == "" {
				continue
			}
			var fields map[string]string
			want(json.Unmarshal([]byte(raw), &fields) == nil, "a line is not a JSON object of strings: %q", raw)
			want(len(fields) == 5 && fields["id"] != "" && fields["from"] != "" && fields["name"] != "" && fields["at"] != "",
				"a line does not hold id, from, name, at and text: %q", raw)
			parsed = append(parsed, fields)
		}
		return parsed
	}
	texts := func(parsed []map[string]string) []string {
		var out []string
		for _, fields := range parsed {
			out = append(out, fields["text"])
		}
		return out
	}
	// watch starts arc message watch --json for bob, and waits until a
	// relay holds the watch.
	watch := func(args ...string) *process {
		t.Helper()
		p := bob.start(append([]string{"message", "watch", "--json"}, args...)...)
		want(p.waitFor("msg=watching", limit), "arc message watch did not watch:\n%s", p.output())
		return p
	}
	// printed waits until a watch printed n lines, and returns them.
	printed := func(p *process, n int) []map[string]string {
		t.Helper()
		deadline := time.Now().Add(limit)
		for len(lines(p.stdout())) < n && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		got := lines(p.stdout())
		want(len(got) >= n, "watch printed %d lines, want %d:\n%s", len(got), n, p.output())
		return got
	}

	// A message arrives while bob watches.
	w := watch()
	before := time.Now().UTC().Add(-time.Second)
	one := send(alice, "one")
	got := printed(w, 1)[0]
	want(got["id"] == one && got["from"] == aliceKey && got["text"] == "one" && got["name"] == alice.name(),
		"watch printed %v; want id %s from %s with the text one", got, one, aliceKey)
	at, err := time.Parse(time.RFC3339, got["at"])
	want(err == nil && strings.HasSuffix(got["at"], "Z") && !at.Before(before.Truncate(time.Second)) && at.Before(time.Now().Add(time.Second)),
		"at is %q; want RFC 3339 UTC near now", got["at"])
	t.Log("ok: watch --json prints the message with its id, key, name, time and text")

	// Other arc commands use bob's home while the watch runs.
	bob.run("message", "send", aliceKey, "reply")
	bob.run("sync")
	inbox := bob.run("message", "inbox", "--json")
	want(inbox == strings.SplitAfter(w.stdout(), "\n")[0], "inbox --json printed %q; watch printed %q", inbox, w.stdout())
	alice.run("sync")
	want(slices.Contains(texts(lines(alice.run("message", "inbox", "--json"))), "reply"), "alice did not get bob's reply")
	t.Log("ok: send, sync and inbox run while watch holds the home; inbox --json prints the line of watch --json")

	want(len(lines(w.stdout())) == 1, "watch printed more than one line:\n%s", w.stdout())
	want(w.exitCode() == 0, "watch did not exit 0 on SIGTERM:\n%s", w.output())
	t.Log("ok: watch printed the message once, and exits 0 on SIGTERM")

	// Two messages arrive while bob does not watch. Bob syncs the first; the
	// relay holds the second.
	two := send(alice, "two")
	bob.run("sync")
	three := send(alice, "three")
	w = watch("--since", one, "--from", aliceKey)
	got2 := printed(w, 2)
	want(got2[0]["id"] == two && got2[1]["id"] == three, "watch --since printed %v; want two, then three", texts(got2))
	t.Log("ok: watch --since prints the missed messages, stored and on the relay")

	// A message from a key outside --from gives no output.
	carol.run("message", "send", bobKey, "from carol")
	deadline := time.Now().Add(limit)
	for !slices.Contains(texts(lines(bob.run("message", "inbox", "--json"))), "from carol") && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	want(slices.Contains(texts(lines(bob.run("message", "inbox", "--json"))), "from carol"), "the watch did not store carol's message")
	four := send(alice, "four")
	got2 = printed(w, 3)
	want(got2[2]["id"] == four, "watch printed %v; want four after three", texts(got2))
	want(slices.Equal(texts(lines(w.stdout())), []string{"two", "three", "four"}), "watch --from printed %v", texts(lines(w.stdout())))
	want(w.exitCode() == 0, "watch did not exit 0 on SIGTERM:\n%s", w.output())
	t.Log("ok: watch --from drops the message from carol, and prints no duplicate")

	// A time includes the messages at that time.
	w = watch("--since", got["at"], "--from", aliceKey)
	printed(w, 4)
	// The inbox does not order the messages of one second.
	all := texts(lines(w.stdout()))
	slices.Sort(all)
	want(slices.Equal(all, []string{"four", "one", "three", "two"}), "watch --since <time> printed %v", texts(lines(w.stdout())))
	want(w.exitCode() == 0, "watch did not exit 0 on SIGTERM:\n%s", w.output())
	t.Log("ok: watch --since <time> prints the stored messages from that time")
}
