package proof

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExecKeep proves kept processes of the exec app through the normal CLI
// and a real relay. A session starts a kept process and ends. The process
// continues. A new session attaches by the name, and gets the output that
// the process wrote while no session was attached, then the exit status.
func TestExecKeep(t *testing.T) {
	work := t.TempDir()
	const limit = 10 * time.Second
	home := func(name string) machine { return arc(t, filepath.Join(work, name)) }

	url := startRelay(t, filepath.Join(work, "relay"))
	for _, name := range []string{"caller", "other", "exec"} {
		home(name).run("keys", "gen")
		home(name).run("relay", "add", url)
	}
	caller, other := home("caller"), home("other")
	execKey := home("exec").key()
	write(t, filepath.Join(work, "exec.json"), `{"grants": ["`+caller.key()+`"], "cwd": "`+work+`"}`+"\n", 0o644)
	server := home("exec").with("EXEC_CONFIG=" + filepath.Join(work, "exec.json")).
		serve("exec://" + program("arc-exec") + "?manifest=" + filepath.Join(repo, "apps/exec/manifest.json"))
	if !server.waitFor("serves exec", limit) {
		t.Fatalf("the exec provider did not serve:\n%s", server.output())
	}
	caller.run("install", execKey, "--yes")
	other.run("install", execKey, "--yes")
	address := "exec+arc://" + execKey + "/"

	// The script writes one tick each 200 ms for 6 seconds, then exits 6.
	script := `i=0; while [ $i -lt 30 ]; do echo tick-$i; i=$((i+1)); sleep 0.2; done; exit 6`
	first := caller.start("session", "--exec", "--timeout", "1m", address, `{"script":"`+script+`","keep":"alfred"}`)
	if !first.waitFor("tick-1\n", limit) {
		t.Fatalf("the kept process wrote nothing:\n%s", first.output())
	}
	first.stop()
	seen := first.output()
	t.Logf("ok: the first session showed %d ticks, then ended", strings.Count(seen, "tick-"))

	list := caller.run("exec", "list")
	if !strings.Contains(list, "alfred") || !strings.Contains(list, "running") {
		t.Fatalf("arc exec list does not show the running process:\n%s", list)
	}
	t.Logf("ok: arc exec list shows the process after its session ended:\n%s", list)

	denied := other.try("", "session", "--exec", "--timeout", "1m", address, `{"attach":"alfred"}`)
	if denied.Err == nil || !strings.Contains(denied.Stderr, "access_denied") {
		t.Fatalf("a caller without a grant attached: %v\n%s%s", denied.Err, denied.Stdout, denied.Stderr)
	}
	t.Log("ok: a caller without a grant gets access_denied")

	// No session is attached for one second. The process writes about five
	// ticks in that time.
	time.Sleep(time.Second)
	attached := caller.try("", "session", "--exec", "--timeout", "1m", address, `{"attach":"alfred"}`)
	if ifaceExit(attached.Err) != 6 {
		t.Fatalf("attach exit = %v, want 6\nstdout: %s\nstderr: %s", attached.Err, attached.Stdout, attached.Stderr)
	}
	var want strings.Builder
	for i := range 30 {
		fmt.Fprintf(&want, "tick-%d\n", i)
	}
	if attached.Stdout != want.String() {
		t.Fatalf("attach output:\n%s\nwant:\n%s", attached.Stdout, want.String())
	}
	detached := 0
	for i := range 30 {
		if !strings.Contains(seen, fmt.Sprintf("tick-%d\n", i)) {
			detached++
		}
	}
	if detached < 3 {
		t.Fatalf("only %d ticks came after the first session ended", detached)
	}
	t.Logf("ok: the attach showed all 30 ticks, %d of them not shown by the first session, and exit status 6", detached)

	if list := caller.run("exec", "list"); strings.Contains(list, "alfred") {
		t.Fatalf("the name stays after the attach showed the exit status:\n%s", list)
	}
	gone := caller.try("", "session", "--exec", "--timeout", "1m", address, `{"attach":"alfred"}`)
	if gone.Err == nil || !strings.Contains(gone.Stderr, "not_found") {
		t.Fatalf("attach to a free name: %v\n%s", gone.Err, gone.Stderr)
	}
	t.Log("ok: the name is free after the exit status was shown")

	sleeper := caller.start("session", "--exec", "--timeout", "1m", address, `{"script":"echo up; sleep 100","keep":"sleeper"}`)
	if !sleeper.waitFor("up", limit) {
		t.Fatalf("the sleeper did not start:\n%s", sleeper.output())
	}
	sleeper.stop()

	// A second attach takes over. The first session ends with detached.
	viewer := caller.start("session", "--exec", "--timeout", "1m", address, `{"attach":"sleeper"}`)
	if !viewer.waitFor("up", limit) {
		t.Fatalf("the attach did not show the buffer:\n%s", viewer.output())
	}
	takeover := caller.start("session", "--exec", "--timeout", "1m", address, `{"attach":"sleeper"}`)
	if !takeover.waitFor("up", limit) || !viewer.waitFor("detached", limit) {
		t.Fatalf("the second attach did not take over:\nfirst: %s\nsecond: %s", viewer.output(), takeover.output())
	}
	t.Logf("ok: a second attach takes over, and the first session ends with: %q", viewer.output())
	takeover.stop()
	if killed := caller.run("exec", "kill", "sleeper"); !strings.HasPrefix(killed, "sleeper\tkilled\t") {
		t.Fatalf("arc exec kill: %q", killed)
	}
	if list := caller.run("exec", "list"); strings.Contains(list, "sleeper") {
		t.Fatalf("the name stays after kill:\n%s", list)
	}
	t.Log("ok: arc exec kill stops a kept process and frees its name")
}
