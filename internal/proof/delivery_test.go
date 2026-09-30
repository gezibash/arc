package proof

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDelivery holds the proofs of phases 1, 2 and 3 of docs/delivery/SPEC.md.
// In phase 1, two homes stand in for two machines of one citizen: they hold
// the same key and separate stores. In phase 2, three citizens hold three
// keys. In phase 3, a provider serves a capability, and an older arc updates
// itself. The steps run in order, and a later step uses the state of an
// earlier one, so the first failure stops the test.
func TestDelivery(t *testing.T) {
	work := t.TempDir()
	begin := time.Now()
	limit := 10 * time.Second

	// want stops the test if a claim does not hold.
	want := func(ok bool, format string, args ...any) {
		t.Helper()
		if !ok {
			t.Fatalf(format, args...)
		}
	}
	// trim drops the final newlines, as $(...) does in a shell.
	trim := func(text string) string { return strings.TrimRight(text, "\n") }
	// version runs one program with --version, and with no home.
	version := func(path string) string {
		output, _ := exec.Command(path, "--version").CombinedOutput()
		return string(output)
	}
	t.Log("ok: arc and the exec provider build")

	a := arc(t, filepath.Join(work, "laptop"))
	b := arc(t, filepath.Join(work, "desktop"))

	// The relay keeps the limits of the public deploy that bear on this proof:
	// the size of an event, and authentication before a gift wrap.
	url := startRelay(t, filepath.Join(work, "relay"), "--max-event-bytes", "262144", "--wrap-auth")
	want(url != "", "the relay did not start")
	t.Logf("ok: a relay listens on %s", url)

	a.run("keys", "gen")
	b.input(read(t, keyfile(t, a.home)), "keys", "add")
	want(a.name()+"\n"+a.key() == b.name()+"\n"+b.key(), "the two machines hold different keys")
	t.Log("ok: two machines hold one key, and separate stores")

	a.run("relay", "add", url)
	b.run("relay", "add", url)

	// The journal is a manifest of interface version 1. Its author announces
	// it, and each machine installs it by trusting that author.
	a.run("announce", filepath.Join(repo, "apps/journal/manifest.json"))
	author := a.key()
	a.run("install", author, "journal", "--yes")
	b.run("install", author, "journal", "--yes")
	t.Log("ok: both machines install the journal")

	// Through a relay.
	a.input("---\ntitle: LR sweep\npage: 1\nnotebook: hrs/ablations\n---\nauc 0.871\nnext: try warmup\n",
		"journal", "write", "hrs/ablations/1", "--title", "LR sweep")
	b.run("sync")
	got := trim(journalDoc(t, begin, b.run("journal", "read", "hrs/ablations/1")))
	want(got == "---\ntitle: LR sweep\npage: 1\nnotebook: hrs/ablations\n---\nauc 0.871\nnext: try warmup",
		"the desktop read %s", got)
	t.Log("ok: a page crosses the relay")

	// The relay holds ciphertext only.
	db, err := os.ReadFile(filepath.Join(work, "relay", "relay.db"))
	want(err == nil, "the relay keeps no relay.db: %v", err)
	want(!bytes.Contains(db, []byte("try warmup")), "the relay holds the page text")
	want(!bytes.Contains(db, []byte("lr-sweep")), "the relay holds the page address")
	t.Log("ok: the relay holds neither the text nor the address")

	// Through a USB stick, with no relay.
	stick := filepath.Join(work, "stick")
	a.run("relay", "rm", url)
	b.run("relay", "rm", url)
	a.input("---\ntitle: Offline\npage: 2\nnotebook: hrs/ablations\n---\nthe stick carried this\n",
		"journal", "write", "hrs/ablations/2")
	a.run("sync", "--dir", stick)
	b.run("sync", "--dir", stick)
	got = trim(journalDoc(t, begin, b.run("journal", "read", "hrs/ablations/2")))
	want(got == "---\ntitle: Offline\npage: 2\nnotebook: hrs/ablations\n---\nthe stick carried this",
		"the desktop did not read the page from the stick")
	t.Log("ok: a page crosses a USB stick")

	// A changed event is refused. The stick first gets every earlier event, so
	// the files that appear after the next write belong to the new page alone.
	stick2 := filepath.Join(work, "stick2")
	a.run("sync", "--dir", stick2)
	before := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(stick2, "events"))
	want(err == nil, "the stick holds no events: %v", err)
	for _, entry := range entries {
		before[entry.Name()] = true
	}
	a.input("---\ntitle: Tamper\npage: 3\nnotebook: hrs/ablations\n---\nthe real text\n",
		"journal", "write", "hrs/ablations/3")
	a.run("sync", "--dir", stick2)
	entries, err = os.ReadDir(filepath.Join(stick2, "events"))
	want(err == nil, "the stick holds no events: %v", err)
	// The change puts one X at the front of the content, once in each line.
	content := regexp.MustCompile(`"content":"[^"]`)
	changed := false
	for _, entry := range entries {
		if before[entry.Name()] {
			continue
		}
		path := filepath.Join(stick2, "events", entry.Name())
		event := read(t, path)
		if !strings.Contains(event, `"kind":31234`) {
			continue
		}
		lines := strings.Split(event, "\n")
		for i, text := range lines {
			if at := content.FindStringIndex(text); at != nil {
				lines[i] = text[:at[1]-1] + "X" + text[at[1]-1:]
			}
		}
		write(t, path, strings.Join(lines, "\n"), 0o644)
		changed = true
	}
	want(changed, "found no draft of the new page to change")
	refused := b.try("", "sync", "--dir", stick2)
	want(strings.Contains(refused.Stderr, "refused"), "the changed event was not refused: %s", refused.Stderr)
	want(!strings.Contains(b.try("", "journal", "read", "hrs/ablations/3").Stdout, "the real text"),
		"the desktop read a changed page")
	t.Log("ok: a changed event is refused, and the page does not read")

	// A large page streams in parts, and a range reads only its parts.
	a.run("relay", "add", url)
	b.run("relay", "add", url)
	var page strings.Builder
	page.WriteString("---\ntitle: Big\npage: 1\nnotebook: hrs/data\n---\n")
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&page, "line %05d %090d\n", i, 0)
	}
	big := page.String()
	a.input(big, "journal", "write", "hrs/data/1")
	back := b.run("journal", "read", "hrs/data/1")
	want(strings.Count(back, "\n") == 3007, "the large page did not read back whole: %d lines", strings.Count(back, "\n"))
	want(journalDoc(t, begin, back) == big, "the large page came back changed")
	t.Logf("ok: a large page of %d bytes crosses the relay in parts", len(big))

	phone := arc(t, filepath.Join(work, "phone"))
	phone.input(read(t, keyfile(t, a.home)), "keys", "add")
	phone.run("relay", "add", url)
	phone.run("install", author, "journal", "--yes")
	got = trim(phone.run("journal", "read", "hrs/data/1", "--lines", "1500:1501"))
	want(got == line(big, 1498)+"\n"+line(big, 1499), "the range read gave %s", got)
	t.Log("ok: a range of two lines reads on a machine that held nothing")

	// Tail streams what is appended. The tail shows nothing before the first
	// write, so a short sleep lets it start.
	tail := b.start("journal", "tail", "hrs/log/1")
	time.Sleep(500 * time.Millisecond)
	a.input("---\ntitle: Live\npage: 1\nnotebook: hrs/log\n---\nfirst note\n", "journal", "write", "hrs/log/1")
	a.run("journal", "append", "hrs/log/1", "second", "note")
	tail.waitFor("second note", limit)
	got = trim(journalDoc(t, begin, tail.output()))
	want(got == "---\ntitle: Live\npage: 1\nnotebook: hrs/log\n---\nfirst note\nsecond note", "tail wrote %s", tail.output())
	tail.stop()
	t.Log("ok: tail streams each note as it is appended")

	t.Log("phase 1 holds: relay, USB stick, refusal, parts, and tail")

	// Phase 2: a message reaches an offline recipient through a third machine
	// that carries a USB stick. Nobody has a relay.
	alice := arc(t, filepath.Join(work, "alice"))
	carol := arc(t, filepath.Join(work, "carol"))
	bob := arc(t, filepath.Join(work, "bob"))
	stickA := filepath.Join(work, "stick-a")
	stickB := filepath.Join(work, "stick-b")

	alice.run("keys", "gen")
	carol.run("keys", "gen")
	// The last line of `keys gen` is the public key.
	made := strings.Split(trim(bob.run("keys", "gen")), "\n")
	bobKey := made[len(made)-1]
	aliceKey := alice.key()
	t.Log("ok: three citizens hold three keys")

	want(strings.Contains(alice.run("message", "send", bobKey, "meet at the river at noon"), "queued"),
		"alice could not queue the message")
	alice.run("sync", "--dir", stickA)
	want(strings.Contains(carol.run("sync", "--dir", stickA), "carried 1"), "carol did not carry the message")
	want(strings.Contains(carol.run("message", "inbox"), "no messages"), "carol could read mail that was not hers")
	carol.run("sync", "--dir", stickB)
	t.Log("ok: carol carries a sealed message that she cannot read")

	for _, dir := range []string{stickA, stickB} {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, secret := range []string{bobKey, aliceKey, "river"} {
				if bytes.Contains(data, []byte(secret)) {
					return fmt.Errorf("%s holds %q", path, secret)
				}
			}
			return nil
		})
		want(err == nil, "a stick names a citizen or holds the text: %v", err)
	}
	t.Log("ok: the sticks name neither citizen and hold no text")

	want(strings.Contains(bob.run("sync", "--dir", stickB), "mail received 1"), "bob did not receive the message")
	inbox := bob.run("message", "inbox")
	want(strings.Contains(inbox, "meet at the river at noon"), "bob's inbox: %s", inbox)
	want(strings.Contains(inbox, alice.name()), "the message does not name alice")
	t.Log("ok: bob receives it, and the seal proves that alice wrote it")

	bob.run("sync", "--dir", stickB)
	carol.run("sync", "--dir", stickB)
	carol.run("sync", "--dir", stickA)
	want(strings.Contains(alice.run("sync", "--dir", stickA), "delivered 1"), "the acknowledgement did not come back")
	outbox := alice.run("message", "outbox")
	want(strings.Contains(outbox, "delivered"), "alice's outbox: %s", outbox)
	t.Log("ok: bob's acknowledgement comes back the same way, and clears alice's outbox")

	// The same message over a relay.
	alice.run("relay", "add", url)
	bob.run("relay", "add", url)
	alice.run("message", "send", bobKey, "and over the relay")
	want(strings.Contains(bob.run("sync"), "mail received 1"), "bob did not receive over the relay")
	want(strings.Contains(alice.run("sync"), "delivered 1"), "the acknowledgement did not cross the relay")
	t.Log("ok: a message and its acknowledgement cross a relay")

	t.Log("phase 2 holds: couriers, route tags, acknowledgements, and the outbox")

	// Phase 3: capabilities. A provider serves exec; a caller finds it,
	// installs it, and calls it live over the relay and by hand through a
	// courier.
	provider := arc(t, filepath.Join(work, "exec"))
	caller := arc(t, filepath.Join(work, "caller"))

	provider.run("keys", "gen")
	provider.run("relay", "add", url)
	providerKey := provider.key()
	providerName := provider.name()
	caller.run("keys", "gen")
	caller.run("relay", "add", url)
	callerKey := caller.key()

	jobs := filepath.Join(work, "jobs")
	stickP := filepath.Join(work, "stick-p")
	stickC := filepath.Join(work, "stick-c")
	for _, dir := range []string{jobs, stickP} {
		want(os.MkdirAll(dir, 0o755) == nil, "could not make %s", dir)
	}
	execConfig := filepath.Join(work, "exec.json")
	write(t, execConfig, fmt.Sprintf(`{"grants": ["%s"], "cwd": "%s", "jobs_dir": "%s"}`+"\n", callerKey, work, jobs), 0o644)
	execURL := "exec://" + program("arc-exec") + "?manifest=" + filepath.Join(repo, "apps/exec/manifest.json")

	serving := provider.with("EXEC_CONFIG="+execConfig).serve(execURL, "--sync-dir", stickP, "--interval", "1s")
	t.Log("ok: a provider serves exec through arc")
	beside := provider.try("", "message", "outbox")
	want(beside.Err == nil, "a second command of the serving identity failed: %s", beside.Stderr)
	t.Log("ok: another command of the serving identity runs while it serves")

	want(strings.Contains(caller.run("discover", "exec"), providerKey), "discover did not find the provider")
	want(strings.Contains(caller.run("install", providerKey, "--yes"), "installed"), "the install failed")
	t.Log("ok: the caller discovers the capability, and installs it")

	// The script waits here, and nothing shows when the provider is ready.
	time.Sleep(500 * time.Millisecond)
	live := caller.try("", "call", providerName, `{"argv":["echo","hello live"]}`)
	want(live.Err == nil, "the live call failed: %s", live.Stderr)
	want(strings.Contains(live.Stdout, "hello live"), "the live call answered %s", live.Stdout)
	trip := regexp.MustCompile(`(?m)^round trip ([^ ]*) via`).FindStringSubmatch(live.Stderr)
	want(trip != nil && trip[1] != "", "the live call recorded no round trip")
	t.Logf("ok: a live call to exec crosses the relay; round trip %s", trip[1])

	stranger := arc(t, filepath.Join(work, "stranger"))
	stranger.run("keys", "gen")
	stranger.run("relay", "add", url)
	stranger.run("install", providerKey, "--yes")
	denied := stranger.try("", "call", providerName, `{"argv":["echo","x"]}`)
	want(denied.Err != nil, "a caller without a grant ran a command")
	want(strings.Contains(denied.Stderr, "access_denied"), "the refusal is not access_denied: %s", denied.Stderr)
	t.Log("ok: a caller without a grant is refused")

	// Store and forward: the caller has no relay now, and Carol carries the
	// call.
	caller.run("relay", "rm", url)
	want(strings.Contains(caller.run("call", providerName, `{"argv":["echo","carried by hand"]}`), "queued"),
		"the call was not queued")
	caller.run("sync", "--dir", stickC)
	carol.run("sync", "--dir", stickC)
	carol.run("sync", "--dir", stickP)
	want(serving.waitFor("answered store-and-forward calls", limit), "the provider did not answer the carried call")
	// The provider puts its reply on the stick at its next sync, 1s later.
	time.Sleep(1500 * time.Millisecond)
	carol.run("sync", "--dir", stickP)
	carol.run("sync", "--dir", stickC)
	caller.run("sync", "--dir", stickC)
	results := caller.run("call", "results")
	want(regexp.MustCompile(`reply:.*carried by hand`).MatchString(results), "the reply did not come back: %s", results)
	t.Log("ok: a store-and-forward call crosses the courier path, and its reply comes back")

	// A provider whose machine paused. The caller keeps a wake hook for it,
	// and arc runs the hook before the live call, the way ssh runs a
	// ProxyCommand.
	serving.stop()
	caller.run("relay", "add", url)

	woke := filepath.Join(work, "woke")
	wokenLog := filepath.Join(work, "serve-woken.log")
	wokenPid := filepath.Join(work, "serve-woken.pid")
	hook := filepath.Join(work, "wake-exec")
	write(t, hook, `#!/bin/sh
# The start script of the provider: serve again, and exit 0 when it listens.
echo woke >> "`+woke+`"
EXEC_CONFIG="`+execConfig+`" "`+program("arc")+`" --home "`+provider.home+`" serve \
  "`+execURL+`" \
  > "`+wokenLog+`" 2>&1 < /dev/null &
echo $! > "`+wokenPid+`"
for _ in $(seq 1 50); do
  grep -q "serves" "`+wokenLog+`" 2>/dev/null && exit 0
  sleep 0.1
done
exit 1
`, 0o755)
	write(t, filepath.Join(caller.home, "wake.toml"),
		fmt.Sprintf("[wake.\"%s\"]\nkind = \"command\"\nargv = [\"%s\"]\n", providerKey, hook), 0o644)
	// The hook starts the provider, so the harness does not stop it.
	t.Cleanup(func() {
		data, err := os.ReadFile(wokenPid)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if syscall.Kill(pid, 0) != nil {
				return
			}
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	woken := caller.try("", "call", providerName, `{"argv":["echo","woken"]}`)
	want(woken.Err == nil, "the call did not wake the provider: %s", woken.Stderr)
	want(strings.Contains(woken.Stdout, "woken"), "the woken provider answered %s", woken.Stdout)
	want(strings.Count(read(t, woke), "\n") == 1, "the hook ran %d times", strings.Count(read(t, woke), "\n"))
	t.Log("ok: arc runs the wake hook, and the woken provider answers")

	want(caller.try("", "call", providerName, `{"argv":["echo","again"]}`).Err == nil, "the second call failed")
	want(strings.Count(read(t, woke), "\n") == 1, "the hook ran again for a provider that answered a moment ago")
	t.Log("ok: arc skips the hook of a provider that answered a moment ago")

	// The NIP-17 claim is not here: `go test ./...` runs the NIP17 tests of core/mail.

	// Updates: a publisher signs a channel with a Nostr key, a releases
	// provider serves it, and an older arc replaces itself with the newer
	// build.
	publisher := arc(t, filepath.Join(work, "publisher"))
	publisher.run("keys", "gen")
	publisherKey := publisher.key()
	releases := filepath.Join(work, "releases")
	for _, dir := range []string{"channels", "blobs"} {
		want(os.MkdirAll(filepath.Join(releases, dir), 0o755) == nil, "could not make %s", dir)
	}
	oldArc := filepath.Join(work, "old", "arc")
	newRoot := filepath.Join(work, "new")
	want(build(oldArc, "./cmd/arc", "-X main.version=0.9.0") == nil, "the old arc did not build")
	err = build(filepath.Join(newRoot, "arc", "bin", "arc"), "./cmd/arc", "-X main.version=9.9.9")
	want(err == nil, "the new arc did not build: %v", err)

	// The archive holds the directory arc, as `tar -czf new.tar.gz -C new arc`.
	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	packed := tar.NewWriter(zipped)
	err = filepath.WalkDir(filepath.Join(newRoot, "arc"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		if header.Name, err = filepath.Rel(newRoot, path); err != nil {
			return err
		}
		if err := packed.WriteHeader(header); err != nil || entry.IsDir() {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(packed, file)
		return err
	})
	want(err == nil, "the archive was not written: %v", err)
	want(packed.Close() == nil && zipped.Close() == nil, "the archive did not close")
	sum := sha256.Sum256(archive.Bytes())
	digest := hex.EncodeToString(sum[:])
	size := archive.Len()
	write(t, filepath.Join(releases, "blobs", digest+".tar.gz"), archive.String(), 0o644)

	unsigned := filepath.Join(work, "unsigned.json")
	write(t, unsigned, fmt.Sprintf(`{"schema_version": 3, "channel": "stable", "publisher": "%s", "sequence": 1,
 "expires_at": %d,
 "releases": [{"version": "9.9.9", "build": "9.9.9+test", "runtime": "go",
   "platform": {"os": "%s", "arch": "%s"},
   "size": %d, "sha256": "%s", "sources": [], "restart_required": true,
   "withdrawn": false, "eligible": true, "install": {"size": %d, "sha256": "%s"}}]}
`, publisherKey, time.Now().Unix()+86400, runtime.GOOS, runtime.GOARCH, size, digest, size, digest), 0o644)
	signed := publisher.try("", "release", "sign", "--root", releases, unsigned)
	want(signed.Err == nil, "the channel was not signed: %s", signed.Stderr)
	want(publisher.try("", "release", "sign", "--root", releases, unsigned).Err != nil, "the same sequence was signed twice")
	t.Log("ok: a publisher signs a channel with a Nostr key, and never the same sequence twice")

	rel := arc(t, filepath.Join(work, "rel"))
	rel.run("keys", "gen")
	rel.run("relay", "add", url)
	relKey := rel.key()
	relServing := rel.with("RELEASES_ROOT=" + releases).serve(
		"exec://" + program("arc-releases") + "?manifest=" + filepath.Join(repo, "apps/releases/manifest.json"))

	old := caller.using(oldArc)
	check := old.try("", "update", "check", "--provider", relKey, "--publisher", publisherKey)
	want(check.Err == nil && strings.Contains(check.Stdout, "names arc 9.9.9"), "update check: %s%s", check.Stdout, check.Stderr)
	apply := old.try("", "update", "apply", "--provider", relKey, "--publisher", publisherKey)
	want(apply.Err == nil, "update apply: %s%s", apply.Stdout, apply.Stderr)
	want(strings.Contains(version(oldArc), "9.9.9"), "the program is %s", version(oldArc))
	want(strings.Contains(version(oldArc+".previous"), "0.9.0"), "the previous program is gone")
	t.Log("ok: an older arc reads the channel over the relay, and replaces itself")

	// Installing the streaming declaration selects core sessions for a second
	// update.
	streamArc := filepath.Join(work, "stream-old", "arc")
	write(t, streamArc, read(t, oldArc+".previous"), 0o755)
	streamOld := caller.using(streamArc)
	want(streamOld.try("", "install", relKey, "--yes").Err == nil, "install streaming releases")
	apply = streamOld.try("", "update", "apply", "--provider", relKey, "--publisher", publisherKey)
	want(apply.Err == nil, "streaming update apply: %s%s", apply.Stdout, apply.Stderr)
	want(strings.Contains(version(streamArc), "9.9.9"), "streaming update did not install the release")
	want(strings.Contains(version(streamArc+".previous"), "0.9.0"), "streaming update lost the old program")
	t.Log("ok: an installed releases provider streams the verified archive through core sessions")

	wrong := old.try("", "update", "check", "--provider", relKey, "--publisher", callerKey)
	want(wrong.Err != nil, "a channel of another publisher passed")
	want(strings.Contains(wrong.Stderr, "another publisher"), "the refusal was %s", wrong.Stderr)
	t.Log("ok: a channel that another key signed is refused")
	relServing.stop()

	t.Log("phase 3 holds: announcements, install, live and carried calls, and updates")
}
