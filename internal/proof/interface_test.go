package proof

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ifaceExit returns the exit status of a command: 0 for no error, and -1 if
// the command did not run to an exit.
func ifaceExit(err error) int {
	if err == nil {
		return 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	}
	return -1
}

// ifaceLine waits for a complete line of output that starts with prefix, and
// returns the rest of that line. It returns "" if no such line comes in time.
func ifaceLine(p *process, prefix string, limit time.Duration) string {
	deadline := time.Now().Add(limit)
	for {
		output := p.output()
		// The last element has no newline yet, so it is not a complete line.
		lines := strings.Split(output, "\n")
		for _, text := range lines[:len(lines)-1] {
			if rest, ok := strings.CutPrefix(text, prefix); ok {
				return strings.TrimSpace(rest)
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ifaceTrim removes the newlines at the end, as $(...) does in a shell.
func ifaceTrim(text string) string { return strings.TrimRight(text, "\n") }

// ifaceLineStarts reports whether a line of text starts with prefix.
func ifaceLineStarts(text, prefix string) bool {
	for one := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(one, prefix) {
			return true
		}
	}
	return false
}

// TestInterface holds the proofs of phases A to D of docs/interface/SPEC.md.
// Two providers announce manifests of interface version 1. A caller installs
// them, and runs their commands as commands of arc, with no code for them in
// arc. The steps share one work directory, and each step uses the state of
// the steps before it.
func TestInterface(t *testing.T) {
	work := t.TempDir()
	// begin is the time before the first write, for the journal timestamps.
	begin := time.Now()
	const limit = 10 * time.Second

	home := func(name string) machine { return arc(t, filepath.Join(work, name)) }
	// contains stops the test if text does not hold part.
	contains := func(text, part, what string) {
		t.Helper()
		if !strings.Contains(text, part) {
			t.Fatalf("%s: want %q in:\n%s", what, part, text)
		}
	}
	// equal stops the test if got, without its last newlines, is not want.
	equal := func(got, want, what string) {
		t.Helper()
		if ifaceTrim(got) != want {
			t.Fatalf("%s:\ngot  %q\nwant %q", what, ifaceTrim(got), want)
		}
	}

	// TestMain builds arc and the providers.
	t.Log("ok: arc and two providers build")

	url := startRelay(t, filepath.Join(work, "relay"))
	t.Logf("ok: a relay listens on %s", url)

	caller := home("caller")
	caller.run("keys", "gen")
	caller.run("relay", "add", url)
	callerKey := caller.key()

	for _, name := range []string{"exec", "sqlite"} {
		home(name).run("keys", "gen")
		home(name).run("relay", "add", url)
	}
	execKey := home("exec").key()
	sqliteKey := home("sqlite").key()

	if err := os.MkdirAll(filepath.Join(work, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	execConfig := "EXEC_CONFIG=" + filepath.Join(work, "exec.json")
	write(t, filepath.Join(work, "exec.json"),
		`{"grants": ["`+callerKey+`"], "cwd": "`+work+`", "jobs_dir": "`+filepath.Join(work, "jobs")+`"}`+"\n", 0o644)
	write(t, filepath.Join(work, "sqlite.json"),
		`{"databases": {"main": {"path": "`+filepath.Join(work, "main.db")+`", "grants": {"`+callerKey+`": "write"}}}}`+"\n", 0o644)

	// Built-in apps serve their canonical interface manifest. The refresh
	// proof below also checks the historical adjacent-interface bundle format.
	execServer := home("exec").with(execConfig).
		serve("exec://" + program("arc-exec") + "?manifest=" + filepath.Join(repo, "apps/exec/manifest.json"))
	sqliteServer := home("sqlite").with("SQLITE_CONFIG=" + filepath.Join(work, "sqlite.json")).
		serve("exec://" + program("arc-sqlite") + "?manifest=" + filepath.Join(repo, "apps/sqlite/manifest.json"))
	if !execServer.waitFor("serves exec", limit) {
		t.Fatalf("the exec provider did not serve:\n%s", execServer.output())
	}
	if !sqliteServer.waitFor("serves sqlite", limit) {
		t.Fatalf("the sqlite provider did not serve:\n%s", sqliteServer.output())
	}
	t.Log("ok: two providers announce manifests of interface version 1")

	install := caller.run("install", execKey, "--yes")
	contains(install, "a service with 5 commands, interface version 1", "the install did not show the manifest")
	contains(install, "installed exec", "exec did not install as exec")
	contains(caller.run("install", sqliteKey, "--as", "db", "--yes"), "installed db", "sqlite did not install as db")
	t.Log("ok: the caller installs exec as exec, and sqlite as db")

	// The proof of phase A.
	equal(caller.run("exec", "run", "echo", "hello"), "hello", "arc exec run echo hello")
	t.Log("ok: arc exec run echo hello answers through the installed manifest")

	contains(caller.run("help", "exec"), "arc exec start <script...>", "help does not list the commands")
	contains(caller.run("exec", "run", "--help"), "usage: arc exec run <argv...>", "a command has no help")
	t.Log("ok: help comes from the manifest")

	got := caller.try("", "exec", "run", "--json", "sh", "-c", "echo out; exit 3")
	contains(got.Stdout, `"exit":3`, "--json wrote")
	if code := ifaceExit(got.Err); code != 3 {
		t.Fatalf("--json exited %d, want the code of the command, 3", code)
	}
	t.Log("ok: --json writes the record, and exits with the code of the command")

	caller.run("db", "create table people (id integer, name text)")
	caller.run("db", "insert into people values (1, 'ada'), (22, 'grace hopper')")
	equal(caller.run("db", "select", "id,", "name", "from", "people", "order", "by", "id"),
		"id  name\n1   ada\n22  grace hopper", "the table")
	t.Log("ok: arc db shows rows as a table")

	// An address names the capability, the provider and the resource.
	execAddress := "exec+arc://" + execKey + "/"
	sqliteAddress := "sqlite+arc://" + sqliteKey + "/main"
	contains(caller.run("call", execAddress, `{"argv":["echo","by address"]}`), "by address", "the call by address failed")
	contains(caller.run("call", "exec+arc://exec/", `{"argv":["echo","by name"]}`), "by name", "the call by an installed name failed")
	contains(caller.run("call", sqliteAddress, `{"sql":"select 22 as n"}`, "--raw"), "[22]",
		"the path of the address did not reach the provider")
	// A path that names no database does not answer.
	caller.refuse("call", "sqlite+arc://"+sqliteKey+"/other", `{"sql":"select 1"}`)
	contains(caller.refuse("call", "exec+arc://"+execKey+"/../x", `{"argv":["true"]}`), "dot segment",
		"the refusal of an address with a dot segment")
	nobody := home("nobody")
	nobody.run("keys", "gen")
	nobody.run("relay", "add", url)
	contains(nobody.refuse("call", execAddress, `{"argv":["true"]}`), "install it first",
		"the refusal of a capability that was not installed")
	t.Log("ok: arc call takes an address: <scheme>+arc://<provider>/<path>")

	// The manifest says how to show the reply of a call by address.
	equal(caller.run("call", execAddress, `{"argv":["echo","shown"]}`), "shown", "the reply was not shown as the service says")
	got = caller.try("", "call", execAddress, `{"argv":["sh","-c","echo out; exit 3"]}`)
	if code := ifaceExit(got.Err); code != 3 || ifaceTrim(got.Stdout) != "out" {
		t.Fatalf("a call by address exited %d, and showed %q; want 3 and %q", code, got.Stdout, "out")
	}
	contains(caller.run("call", execAddress, `{"argv":["echo","raw"]}`, "--raw"), `"stdout":"raw\n"`,
		"--raw did not write the reply as it came")
	equal(caller.run("call", sqliteAddress, `{"sql":"select 22 as n"}`), "n\n22", "the sqlite reply is not a table")
	t.Log("ok: a call by address shows the reply as the service says, and --raw as it came")

	// A provider announces a new manifest. The next call uses it, although
	// this machine holds the older announcement.
	legacy := read(t, filepath.Join(repo, "runtime/capability/testdata/exec-legacy.json"))
	current := read(t, filepath.Join(repo, "apps/exec/manifest.json"))
	for _, version := range []string{"old", "new"} {
		write(t, filepath.Join(work, "exec2-"+version, "manifest.json"), legacy, 0o644)
	}
	// The old interface is the current one without the output of the service.
	var old map[string]any
	if err := json.Unmarshal([]byte(current), &old); err != nil {
		t.Fatal(err)
	}
	service, ok := old["service"].(map[string]any)
	if !ok {
		t.Fatalf("apps/exec/manifest.json holds no service object")
	}
	delete(service, "output")
	without, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(work, "exec2-old", "interface.json"), string(without)+"\n", 0o644)
	write(t, filepath.Join(work, "exec2-new", "interface.json"), current, 0o644)
	exec2 := home("exec2")
	exec2.run("keys", "gen")
	exec2.run("relay", "add", url)
	exec2Key := exec2.key()
	serveExec2 := func(version string) *process {
		t.Helper()
		// An announcement carries its time in seconds. Of two in one second,
		// NIP-01 keeps the lower ID, so each announcement comes a second later.
		time.Sleep(time.Second)
		return exec2.with(execConfig).
			serve("exec://" + program("arc-exec") + "?manifest=" + filepath.Join(work, "exec2-"+version, "manifest.json"))
	}
	// Each form of call meets the new manifest: first by provider, then by
	// address. Each round starts from the old manifest.
	for round, target := range []string{exec2Key, "exec+arc://" + exec2Key + "/"} {
		server := serveExec2("old")
		if round == 0 {
			caller.run("install", exec2Key, "--as", "exec2", "--yes")
		}
		contains(caller.run("call", target, `{"argv":["echo","old"]}`), `"stdout"`,
			target+": the old manifest has no output, so the reply must come as it came")
		server.stop()
		server = serveExec2("new")
		equal(caller.run("call", target, `{"argv":["echo","new"]}`), "new", target+": the call used the older announcement")
		server.stop()
	}
	t.Log("ok: a call uses the newest announcement of the provider")

	contains(caller.refuse("exec", "run"), "missing <argv...>", "the error of a missing argument")
	contains(caller.refuse("exec", "fly"), `no command "fly"`, "the error of an unknown command")
	t.Log("ok: arc checks the arguments before it calls")

	contains(caller.refuse("install", execKey, "--as", "relay", "--yes"), "is a command of arc",
		"the error of a capability that takes the name of a command")
	t.Log("ok: a capability cannot take the name of a command of arc")

	stranger := home("stranger")
	stranger.run("keys", "gen")
	stranger.run("relay", "add", url)
	stranger.run("install", execKey, "--yes")
	contains(stranger.refuse("exec", "run", "id"), "access_denied", "the refusal of a caller without a grant")
	t.Log("ok: a caller without a grant is refused")

	got = caller.try("", "exec", "run", "sh", "-c", "echo out; exit 3")
	if code := ifaceExit(got.Err); code != 3 {
		t.Fatalf("exec run of a command that exits 3 exited %d: %s", code, got.Stderr)
	}
	equal(got.Stdout, "out", "exec run lost the output")
	if got.Stderr != "" {
		t.Fatalf("exec run wrote an error for a code: %s", got.Stderr)
	}
	t.Log("ok: exec run exits with the code of the command, after its output")

	got = caller.try("", "exec", "run", "--later", "echo", "later")
	if got.Err != nil {
		t.Fatalf("exec run --later: %v\n%s", got.Err, got.Stderr)
	}
	contains(got.Stderr, "queued", "--later did not queue")
	t.Log("ok: --later queues a live call in the outbox")

	// The starter bundle of arc apps init announces its interface.json, and a
	// caller runs its command. As in the script, apps init runs with no home.
	bot := filepath.Join(work, "weather-bot")
	initialize := exec.Command(program("arc"), "apps", "init", bot)
	initialize.Dir = repo
	if output, err := initialize.CombinedOutput(); err != nil {
		t.Fatalf("arc apps init: %v\n%s", err, output)
	}
	starter := home("starter")
	starter.run("keys", "gen")
	starter.run("relay", "add", url)
	starterKey := starter.key()
	starterServer := starter.serve(bot)
	if !starterServer.waitFor("serves weather-bot", limit) {
		t.Fatalf("the starter did not serve:\n%s", starterServer.output())
	}
	contains(caller.run("install", starterKey, "--yes"), "installed weather-bot", "the starter did not install")
	equal(caller.run("weather-bot", "say", "hello", "from", "arc"), "hello from weather-bot: hello from arc", "the starter")
	starterServer.stop()
	t.Log("ok: arc weather-bot say hello answers through the starter of arc apps init")

	t.Log("phase A holds: manifests, arguments, templates, call, format")

	// Phase B: sealed data. The journal and the files are manifests, with no
	// code for them in arc. Two machines hold one key.
	laptop := caller
	desktop := home("desktop")
	desktop.input(read(t, keyfile(t, caller.home)), "keys", "add")
	desktop.run("relay", "add", url)

	laptop.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	laptop.run("announce", filepath.Join(repo, "apps/files/manifest.json"))
	for _, one := range []machine{laptop, desktop} {
		one.run("install", callerKey, "journal", "--yes")
		one.run("install", callerKey, "files", "--yes")
	}
	t.Log("ok: two machines install the journal and the files from their author")

	laptop.input("---\ntitle: LR sweep\npage: 1\nnotebook: hrs/ablations\n---\nauc 0.871\n",
		"journal", "write", "hrs/ablations/1", "--title", "LR sweep")
	laptop.run("journal", "append", "hrs/ablations/1", "next:", "try", "warmup")
	equal(journalDoc(t, begin, desktop.run("journal", "read", "hrs/ablations/1")),
		"---\ntitle: LR sweep\npage: 1\nnotebook: hrs/ablations\n---\nauc 0.871\nnext: try warmup", "the desktop read")
	contains(desktop.run("journal", "ls"), "hrs/ablations/1\tLR sweep", "journal ls")
	t.Log("ok: a page that is written and appended on one machine reads on the other")

	// Two commands of one identity run at once: tail holds a watch, and
	// append writes on the same machine.
	tail := laptop.start("journal", "tail", "hrs/ablations/1")
	// As in the script, the wait for the first lines of the tail is no check.
	tail.waitFor("next: try warmup", limit)
	if got := laptop.try("", "journal", "append", "hrs/ablations/1", "beside", "the", "tail"); got.Err != nil {
		t.Fatalf("a second command of one identity failed: %v\n%s", got.Err, got.Stderr)
	}
	if !tail.waitFor("beside the tail", limit) {
		t.Fatalf("the tail did not show the append:\n%s", tail.output())
	}
	tail.stop()
	t.Log("ok: two commands of one identity run at once")

	revisions := 0
	history := desktop.run("journal", "history", "hrs/ablations/1")
	for one := range strings.SplitSeq(history, "\n") {
		if strings.HasPrefix(one, "20") {
			revisions++
		}
	}
	if revisions != 3 {
		t.Fatalf("the history holds %d lines that start with a year, want 3:\n%s", revisions, history)
	}
	if found := desktop.run("journal", "search", "warmup"); !ifaceLineStarts(found, "hrs/ablations/1") {
		t.Fatalf("search found nothing:\n%s", found)
	}
	t.Log("ok: the page has two revisions, and search finds it")

	laptop.run("journal", "kpi", "set", "hrs", "auc", "0.85")
	laptop.run("journal", "kpi", "set", "hrs", "auc", "0.87", "--note", "warmup")
	contains(desktop.run("journal", "kpi", "latest", "hrs"), "auc\t0.87\twarmup", "kpi latest")
	if log := desktop.run("journal", "kpi", "log", "hrs", "auc"); strings.Count(log, "\n") != 2 {
		t.Fatalf("kpi log shows %d lines, want 2:\n%s", strings.Count(log, "\n"), log)
	}
	t.Log("ok: a KPI keeps its last value and its log")

	weights := make([]byte, 200000)
	if _, err := rand.Read(weights); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "weights.bin"), weights, 0o644); err != nil {
		t.Fatal(err)
	}
	id := ifaceTrim(laptop.run("files", "put", filepath.Join(work, "weights.bin")))
	contains(desktop.run("files", "list"), "weights.bin\t200000 bytes", "files list")
	desktop.run("files", "get", id, "--output", filepath.Join(work, "weights-back.bin"))
	back, err := os.ReadFile(filepath.Join(work, "weights-back.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(weights, back) {
		t.Fatalf("the file came back changed: %d bytes, want the same 200000 bytes", len(back))
	}
	t.Log("ok: a binary file of 200000 bytes crosses the relay in parts, and its hash checks")

	// A stick made before the delete must not bring the page back.
	oldStick := filepath.Join(work, "old-stick")
	laptop.run("sync", "--dir", oldStick)
	laptop.run("journal", "delete", "hrs/ablations/1")
	if page := desktop.try("", "journal", "read", "hrs/ablations/1").Stdout; page != "" {
		t.Fatalf("the desktop still reads the deleted page:\n%s", page)
	}
	desktop.run("sync", "--dir", oldStick)
	if page := desktop.try("", "journal", "read", "hrs/ablations/1").Stdout; page != "" {
		t.Fatalf("an old stick brought the deleted page back:\n%s", page)
	}
	contains(desktop.run("journal", "history", "hrs/ablations/1"), "no revisions", "the history outlived the delete")
	t.Log("ok: delete removes the page and its history, and an old stick does not bring it back")

	// With no relay, the page crosses a stick.
	stick := filepath.Join(work, "stick")
	laptop.run("relay", "rm", url)
	desktop.run("relay", "rm", url)
	laptop.input("---\ntitle: Offline\npage: 1\nnotebook: hrs/notes\n---\ncarried by hand\n", "journal", "write", "hrs/notes/1")
	laptop.run("sync", "--dir", stick)
	desktop.run("sync", "--dir", stick)
	equal(journalDoc(t, begin, desktop.run("journal", "read", "hrs/notes/1")),
		"---\ntitle: Offline\npage: 1\nnotebook: hrs/notes\n---\ncarried by hand", "the stick did not carry the page")
	t.Log("ok: with no relay, a page crosses a USB stick")

	t.Log("phase B holds: drafts, checkpoints, parts, delete, the journal and the files")

	// Phase C: private kinds through the mail layer, and a NIP-29 group on a
	// relay that enforces it. Direct messages and Agora are manifests.
	laptop.run("relay", "add", url)
	bob := home("bob")
	moderator := home("moderator")
	bob.run("keys", "gen")
	bob.run("relay", "add", url)
	bobKey := bob.key()
	moderator.run("keys", "gen")
	moderator.run("relay", "add", url)
	moderatorKey := moderator.key()

	boardRelay := home("board").start("relay", "serve", "--listen", "127.0.0.1:0", "--group", "agora", "--admin", moderatorKey)
	board := ifaceLine(boardRelay, "relay listens on ", limit)
	if board == "" {
		t.Fatalf("the board relay did not start:\n%s", boardRelay.output())
	}
	if !boardRelay.waitFor("hosts groups agora", limit) {
		t.Fatalf("the board hosts no group:\n%s", boardRelay.output())
	}
	t.Log("ok: a relay hosts the NIP-29 group agora, with one admin")

	// The author of Agora names the board relay in the manifest.
	agora := filepath.Join(work, "agora.json")
	write(t, agora, strings.ReplaceAll(read(t, filepath.Join(repo, "apps/agora/manifest.json")), "wss://board.example", board), 0o644)
	laptop.run("announce", filepath.Join(repo, "apps/dm/manifest.json"))
	laptop.run("announce", agora)
	for _, one := range []machine{laptop, bob, moderator} {
		one.run("install", callerKey, "dm", "--yes")
		one.run("install", callerKey, "agora", "--yes")
	}
	t.Log("ok: three citizens install dm and agora")

	laptop.run("dm", "send", bobKey, "meet", "at", "noon")
	contains(bob.run("dm", "inbox"), "meet at noon", "bob's inbox")
	bob.run("dm", "send", callerKey, "see", "you", "there")
	conversation := laptop.run("dm", "open", bobKey)
	contains(conversation, "meet at noon", "the conversation")
	contains(conversation, "see you there", "the conversation")
	// The script also runs the NIP17 tests of ./core/mail/; go test ./... runs them.
	t.Log("ok: a direct message crosses the relay, both ways, and opens in a NIP-17 client")

	// A list stands for its members where a command takes a key.
	laptop.refuse("lists", "add", "dm", "pals", "not-a-key")
	laptop.refuse("lists", "add", "nothing", "pals", bobKey)
	laptop.run("lists", "add", "dm", "pals", bobKey, moderatorKey)
	if list := laptop.run("lists", "ls", "dm", "pals"); strings.Count(list, "\n") != 2 {
		t.Fatalf("the list holds %d lines, want 2:\n%s", strings.Count(list, "\n"), list)
	}
	laptop.run("dm", "send", "pals", "hello", "to", "the", "list")
	contains(bob.run("dm", "inbox"), "hello to the list", "bob did not receive the message to the list")
	contains(moderator.run("dm", "inbox"), "hello to the list", "the moderator did not receive the message to the list")
	laptop.run("lists", "rm", "dm", "pals")
	contains(laptop.run("lists", "ls", "dm"), "no lists", "the removed list still shows")
	t.Log("ok: a list of two citizens sends one message to each")

	// The post prints its address as the last line of standard error.
	got = laptop.try("", "agora", "post", "--title", "Hello", "first", "post")
	if got.Err != nil {
		t.Fatalf("agora post: %v\n%s", got.Err, got.Stderr)
	}
	errLines := strings.Split(ifaceTrim(got.Stderr), "\n")
	post := errLines[len(errLines)-1]
	if !strings.HasPrefix(post, "nevent1") {
		t.Fatalf("the post printed %q, want an address that starts with nevent1", post)
	}
	contains(bob.run("agora", "feed"), "Hello", "bob's feed")
	bob.run("agora", "reply", post, "welcome")
	contains(laptop.run("agora", "thread", post), "welcome", "the thread")
	t.Log("ok: a post and a reply cross the board relay")

	contains(bob.refuse("agora", "remove", post), "only an admin", "the refusal of a citizen who is not an admin")
	moderator.run("agora", "remove", post)
	contains(bob.run("agora", "feed"), "no posts", "the removed post still shows")
	// The script also runs the NIP29 tests of ./adapters/relay/groups/; go test ./... runs them.
	t.Log("ok: only the admin removes the post, and the post opens in a NIP-29 client")

	t.Log("phase C holds: private kinds, NIP-29 groups, direct messages and Agora")

	// Phase D: what a capability can do stays what the citizen agreed to, and
	// a key can be sealed, or held by a remote signer.
	dm := read(t, filepath.Join(repo, "apps/dm/manifest.json"))
	const message = `"kind": 14, "visibility": "private"}`
	profile := filepath.Join(work, "dm-profile.json")
	write(t, profile, strings.ReplaceAll(dm, message, message+`, "profile": {"kind": 0, "visibility": "public"}`), 0o644)
	contains(laptop.refuse("announce", profile), "reserved", "the refusal of a manifest that names a reserved kind")
	t.Log("ok: a manifest that names a reserved kind does not announce")

	note := filepath.Join(work, "dm-note.json")
	write(t, note, strings.ReplaceAll(dm, message, message+`, "note": {"kind": 1, "visibility": "public"}`), 0o644)
	// The new announcement must carry a later second than the one before it.
	time.Sleep(time.Second)
	laptop.run("announce", note)
	consent := bob.refuse("dm", "inbox")
	contains(consent, "changed what dm can do", "the refusal of a new kind without consent")
	contains(consent, "kind 1 (note)", "the refusal of a new kind without consent")
	contains(bob.run("install", callerKey, "dm", "--yes"), "posted in public, signed by you", "install did not show the new kind")
	contains(bob.run("dm", "inbox"), "meet at noon", "the inbox after consent")
	t.Log("ok: a new version that adds a public kind stops until the citizen installs again")

	dry := laptop.input("---\ntitle: Draft\npage: 2\nnotebook: hrs/notes\n---\n", "journal", "write", "hrs/notes/2", "--dry-run")
	contains(dry, `"kind": 30023`, "the dry run")
	if page := laptop.try("", "journal", "read", "hrs/notes/2").Stdout; page != "" {
		t.Fatalf("a dry run wrote the page:\n%s", page)
	}
	t.Log("ok: --dry-run shows the event, and signs nothing")

	sealed := home("sealed")
	opened := sealed.with("ARC_PASSPHRASE=correct horse")
	identity := opened.run("keys", "gen", "--encrypt")
	if !strings.HasPrefix(read(t, keyfile(t, sealed.home)), "ncryptsec1") {
		t.Fatal("the key file is not an ncryptsec")
	}
	whoami := strings.SplitN(sealed.run("whoami"), "\n", 3)
	if len(whoami) < 2 || whoami[0]+"\n"+whoami[1] != ifaceTrim(identity) {
		t.Fatalf("the sealed key is another identity:\nwhoami   %q\nkeys gen %q", whoami, identity)
	}
	opened.run("message", "outbox")
	sealed.with("ARC_PASSPHRASE=wrong").refuse("message", "outbox")
	t.Log("ok: a key sealed with a passphrase opens with it, and not without it")

	// An agent signs through its owner's bunker, and holds no secret key.
	owner := home("owner")
	agent := home("agent")
	owner.run("keys", "gen")
	ownerKey := owner.key()
	// The owner allows posts, drafts and their parts, seals, and relay lists;
	// not replies.
	bunker := owner.start("keys", "bunker", "--relay", url,
		"--allow-kind", "11", "--allow-kind", "31234", "--allow-kind", "1234", "--allow-kind", "3275",
		"--allow-kind", "13", "--allow-kind", "10050", "--allow-kind", "10013")
	uri := ifaceLine(bunker, "bunker://", limit)
	if uri == "" {
		t.Fatalf("the bunker did not start:\n%s", bunker.output())
	}
	uri = "bunker://" + uri
	added := strings.Split(ifaceTrim(agent.run("keys", "add", uri)), "\n")
	if added[len(added)-1] != ownerKey {
		t.Fatalf("the agent is %q, not the owner %s", added, ownerKey)
	}
	secret := strings.TrimSpace(read(t, keyfile(t, owner.home)))
	err = filepath.WalkDir(agent.home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		if strings.Contains(read(t, path), secret) {
			t.Fatalf("the agent holds the owner's secret key in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.run("relay", "add", url)
	for _, capability := range []string{"agora", "journal", "dm"} {
		agent.run("install", callerKey, capability, "--yes")
	}
	t.Log("ok: an agent signs as its owner through a bunker, and holds no secret key")

	agent.run("agora", "post", "--title", "From the agent", "signed", "remotely")
	feed := bob.run("agora", "feed")
	contains(feed, "From the agent", "the agent's post is not on the board")
	contains(feed, owner.name(), "the post does not name the owner")
	agentPost := regexp.MustCompile(`nevent1[a-z0-9]*`).FindString(feed)
	contains(agent.refuse("agora", "reply", agentPost, "not", "allowed"), "does not sign kind 1111",
		"the refusal of a kind that the bunker does not allow")
	t.Log("ok: the bunker signs the kinds its owner allows, and refuses the rest")

	const agentPage = "---\ntitle: Agent notes\npage: 1\nnotebook: ops/agent\n---\nwritten by the agent"
	agent.input(agentPage+"\n", "journal", "write", "ops/agent/1")
	equal(journalDoc(t, begin, agent.run("journal", "read", "ops/agent/1")), agentPage, "the agent read")
	owner.run("relay", "add", url)
	owner.run("install", callerKey, "journal", "--yes")
	equal(journalDoc(t, begin, owner.run("journal", "read", "ops/agent/1")), agentPage, "the owner, with the key, read")
	t.Log("ok: an agent keeps a journal through the bunker, and its owner reads the same page with the key")

	// By default the bunker decrypts only what its owner sealed to
	// themselves: the journal works, and incoming mail stays shut.
	agent.run("dm", "send", bobKey, "hello", "from", "the", "agent")
	inbox := bob.run("dm", "inbox")
	contains(inbox, "hello from the agent", "bob's inbox")
	contains(inbox, owner.name(), "the message does not come from the owner")
	bob.run("dm", "send", ownerKey, "hello", "agent")
	got = agent.try("", "dm", "inbox")
	if got.Err == nil {
		t.Fatal("the inbox succeeded although the bunker refused to open its recorded mail")
	}
	if strings.Contains(got.Stdout, "hello agent") {
		t.Fatalf("the bunker opened mail by default:\n%s", got.Stdout)
	}
	contains(got.Stderr, "decrypts only what its owner sealed", "the agent was not told why")
	t.Log("ok: by default the bunker opens the owner's own drafts, and not their mail")

	bunker.stop()
	bunker = owner.start("keys", "bunker", "--relay", url, "--decrypt", "all", "--allow-kind", "13")
	if again := ifaceLine(bunker, "bunker://", limit); "bunker://"+again != uri {
		t.Fatalf("the bunker URI changed on restart:\ngot  %q\nwant %q", "bunker://"+again, uri)
	}
	contains(agent.run("dm", "inbox"), "hello agent", "the agent's inbox")
	t.Log("ok: with --decrypt all, the agent reads its owner's mail, and the URI stays the same")

	t.Log("phase D holds: reserved kinds, consent, dry runs, sealed keys, and remote signers")
}
