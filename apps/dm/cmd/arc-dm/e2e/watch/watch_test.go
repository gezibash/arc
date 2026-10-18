package watch_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gezibash/arc/apps/dm/cmd/arc-dm/internal/dmtest"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/arcprogram/arcprogramtest"
)

func TestMain(m *testing.M) { dmtest.Main(m) }

// A watch shows the short id and the quote of a reply, also when the
// parent is a message that this citizen sent after the watch started. A
// reply whose parent is not here shows the id only.
func TestAWatchQuotesTheParent(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "hello")
	bob.Sync()

	watch := bob.Command("watch", "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	bob.DM("send", ada.Name, "bring the map")
	ada.Sync()
	short := dmtest.ShortID(t, ada.DM("open", bob.Name), "bring the map")
	ada.DM("reply", short, "yes")
	ada.ArcSend(bob.Key, "and that", "--arctest-tag", "e,"+strings.Repeat("7a", 32))
	want := []string{"\n↳ " + short + "  you: bring the map\n  yes\n", "\n↳ 7a7a7a7a7a7a  (not on this device)\n  and that\n"}
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool {
		return strings.Contains(stdout.String(), want[0]) && strings.Contains(stdout.String(), want[1])
	}) {
		t.Errorf("the watch printed:\n%s\nwant\n%s", stdout.String(), strings.Join(want, ""))
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
}

// A watch reads the outbox at most once for each parent that it does not
// hold, and at most once each 5 s, however many ids that no
// message has a sender names.
func TestAWatchReadsTheOutboxRarely(t *testing.T) {
	t.Parallel()
	mallory, bob := dmtest.NewPerson(t, "mallory"), dmtest.NewPerson(t, "bob")
	mallory.DM("send", bob.Key, "hello")
	bob.Sync()

	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	wrapper := filepath.Join(dir, "arc")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + runs + "\nexec " + bob.Arc.Program + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	watch := exec.Command(dmtest.Program(), "watch", "-arc", wrapper, "-home", bob.Arc.Home, "-state", bob.State, "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	start := time.Now()
	const junk = 20
	for i := range junk {
		mallory.ArcSend(bob.Key, fmt.Sprintf("junk %d", i), "--arctest-tag", fmt.Sprintf("e,%064x", i+1))
	}
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Count(stdout.String(), "(not on this device)") == junk }) {
		t.Fatalf("the watch printed:\n%s", stdout.String())
	}
	elapsed := time.Since(start)
	data, _ := os.ReadFile(runs)
	// One read of the outbox at the start, then at most one each
	// 5 s, the first at once.
	most := 2 + int(elapsed/dmtest.RefreshEvery)
	if n := strings.Count(string(data), "message outbox"); n < 2 || n > most || n >= junk {
		t.Errorf("the watch read the outbox %d times in %v, want 2 to %d:\n%s", n, elapsed, most, data)
	}
	if n := strings.Count(string(data), "message inbox"); n != 1 {
		t.Errorf("the watch read the inbox %d times, want 1:\n%s", n, data)
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
}

// A second interrupt ends the watch at once, while a read of the outbox
// hangs: the read at the end of the watch, or a timed read that hung
// before the first interrupt. The first interrupt ends a timed read; the
// second ends the last one. The watch shows its held reply each time.
func TestASecondInterruptEndsTheWatch(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"the last read hangs", "a timed read hangs"} {
		t.Run(how, func(t *testing.T) { secondInterrupt(t, how) })
	}
}

func secondInterrupt(t *testing.T, how string) {
	timed := how == "a timed read hangs"
	mallory, bob := dmtest.NewPerson(t, "mallory"), dmtest.NewPerson(t, "bob")
	mallory.DM("send", bob.Key, "hello")
	bob.Sync()
	dir := t.TempDir()
	count, wrapper := filepath.Join(dir, "count"), filepath.Join(dir, "arc")
	// Each read of the outbox after the first hangs: the arc that arc-dm
	// runs sleeps.
	sleep := fmt.Sprintf("%d.%d", 30+os.Getpid()%30, time.Now().Nanosecond()%1000)
	script := "#!/bin/sh\ncase " + `"$*"` + " in *" + `"message outbox"` + "*) echo x >> " + count + "; [ $(wc -l < " + count + ") -ge 2 ] && exec sleep " + sleep + ";; esac\nexec " + bob.Arc.Program + " " + `"$@"` + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	reads := func() int {
		data, _ := os.ReadFile(count)
		return strings.Count(string(data), "x")
	}
	watch := exec.Command(dmtest.Program(), "watch", "-arc", wrapper, "-home", bob.Arc.Home, "-state", bob.State, "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	defer func() { _ = exec.Command("pkill", "-f", "sleep "+sleep).Run() }()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	mallory.ArcSend(bob.Key, "junk", "--arctest-tag", "e,"+strings.Repeat("7a", 32))
	// The watch stores each message before it prints it: once the inbox
	// holds the reply, the watch holds it too, a moment later.
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool {
		inbox, _ := bob.Arc.Inbox(context.Background())
		return slices.ContainsFunc(inbox, func(m arcprogram.Message) bool { return m.Text == "junk" })
	}) {
		t.Fatal("the reply did not arrive")
	}
	if timed {
		// The reply waits; the timed read of the outbox hangs.
		if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return reads() >= 2 }) {
			t.Fatal("the watch read the outbox no second time")
		}
	} else {
		time.Sleep(200 * time.Millisecond)
	}
	before := reads()
	first := time.Now()
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	// The end of the watch reads the outbox once more, and that read
	// hangs too. It is the only read that can start after the first
	// interrupt, so it also shows that arc-dm took that interrupt. Only
	// then does the test send the second: two signals of one kind that
	// wait together become one, and under load a timed read can have
	// made the count before the first interrupt.
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return reads() > before }) {
		t.Fatalf("the end of the watch did not read the outbox; reads %d", reads())
	}
	// The first interrupt ends a timed read at once, not when its own
	// limit of 10 s ends it.
	if took := time.Since(first); took > dmtest.ReadTimeout/2 {
		t.Errorf("the end of the watch read the outbox %v after the first interrupt, want under %v", took, dmtest.ReadTimeout/2)
	}
	// A timed read that starts between the count and the first interrupt
	// also raises the count: give arc-dm a moment to take the interrupt, so
	// the second does not come at the same instant.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = watch.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		// Still running: make Go show where each goroutine of arc-dm
		// waits, then fail with it. Under the race detector, a program
		// that ends can take more than a second to exit.
		readsThen := reads()
		processes, _ := exec.Command("pgrep", "-fl", bob.Arc.Home).Output()
		_ = watch.Process.Signal(syscall.SIGQUIT)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		t.Fatalf("the watch went on %v after a second interrupt; reads of the outbox %d; processes of the home:\n%s\nstderr, with the goroutines of arc-dm:\n%s", time.Since(start), readsThen, processes, stderr.String())
	}
	if !strings.Contains(stdout.String(), "  junk\n") {
		t.Errorf("the watch dropped the held reply:\n%s", stdout.String())
	}
	if out, _ := exec.Command("pgrep", "-f", "sleep "+sleep).Output(); len(out) > 0 {
		t.Errorf("a hung arc is still running: %s", out)
	}
}

// A reply whose parent the watch does not hold waits for the next read of
// the outbox, and is not shown without its parent. A reply to an id that
// no message has, then within 5 s a reply to a message that bob sent from
// another process after the watch started: the second still shows its
// parent, and the replies keep their order.
func TestAWatchHoldsAReplyUntilItReadsTheOutbox(t *testing.T) {
	t.Parallel()
	ada, bob, mallory := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob"), dmtest.NewPerson(t, "mallory")
	ada.DM("send", bob.Key, "hello")
	mallory.DM("send", bob.Key, "hello too")
	bob.Sync()

	watch := bob.Command("watch", "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	mallory.ArcSend(bob.Key, "junk", "--arctest-tag", "e,"+strings.Repeat("7a", 32))
	bob.DM("send", ada.Name, "bring the map")
	ada.Sync()
	short := dmtest.ShortID(t, ada.DM("open", bob.Name), "bring the map")
	ada.DM("reply", short, "yes")
	want := "\n\u21b3 " + short + "  you: bring the map\n  yes\n"
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stdout.String(), want) }) {
		t.Fatalf("the watch printed:\n%s\nwant\n%s", stdout.String(), want)
	}
	if out := stdout.String(); strings.Index(out, "  junk\n") > strings.Index(out, want) || strings.Contains(out, short+"  (not on this device)") {
		t.Errorf("the watch printed:\n%s", out)
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
}

// A reply that waits for the outbox when the watch ends still shows its
// parent: the watch reads the outbox once more as it ends.
func TestAWatchShowsAHeldReplyAtItsEnd(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "hello")
	bob.Sync()

	watch := bob.Command("watch", "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	started := time.Now()
	bob.DM("send", ada.Name, "bring the map")
	ada.Sync()
	short := dmtest.ShortID(t, ada.DM("open", bob.Name), "bring the map")
	ada.DM("reply", short, "yes")
	// The watch stores each message before it prints it: once the inbox
	// holds the reply, the watch holds it too, a moment later. It holds it
	// until 5 s after it started; the test ends the watch before then,
	// when it can.
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool {
		inbox, _ := bob.Arc.Inbox(context.Background())
		return slices.ContainsFunc(inbox, func(m arcprogram.Message) bool { return m.Text == "yes" })
	}) {
		t.Fatal("the reply did not arrive")
	}
	time.Sleep(200 * time.Millisecond)
	if time.Since(started) > dmtest.RefreshEvery {
		t.Logf("the machine is slow: the watch read the outbox before its end, %v after its start", time.Since(started))
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
	if want := "\n\u21b3 " + short + "  you: bring the map\n  yes\n"; !strings.Contains(stdout.String(), want) {
		t.Errorf("the watch printed:\n%s\nwant\n%s", stdout.String(), want)
	}
}

// A key that a passphrase seals asks on the terminal of arc-dm, with no
// echo, and arc-dm works: arc runs in the process group of arc-dm, which
// the terminal lets read.
func TestASealedKeyAsksOnTheTerminal(t *testing.T) {
	t.Parallel()
	bob := dmtest.NewPerson(t, "bob")
	seal := bob.Arc.Command(context.Background(), "keys", "encrypt")
	seal.Env = append(os.Environ(), "ARC_PASSPHRASE=open sesame")
	if out, err := seal.CombinedOutput(); err != nil {
		t.Fatalf("arc keys encrypt: %v\n%s", err, out)
	}
	command, terminal, out := dmtest.OnTerminal(t, "ls", "-arc", bob.Arc.Program, "-home", bob.Arc.Home, "-state", bob.State)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	answered := 0
	deadline := time.After(dmtest.WaitLimit)
	for {
		select {
		case err := <-done:
			if err != nil || answered == 0 || strings.Contains(out.String(), "open sesame") {
				t.Errorf("arc-dm ls ended with %v after %d passphrases; the terminal showed:\n%s", err, answered, out.String())
			}
			return
		case <-deadline:
			t.Fatalf("arc-dm ls did not end after %d passphrases; the terminal showed:\n%s", answered, out.String())
		case <-time.After(100 * time.Millisecond):
			if n := strings.Count(out.String(), "passphrase of the key: "); n > answered {
				answered = n
				if _, err := terminal.Write([]byte("open sesame\r")); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// A hangup, as when the terminal of arc-dm closes, ends arc-dm and each
// arc that it runs: sent to the process of arc-dm alone, and to its
// process group, as the terminal sends it. (A test cannot close its end of
// a pseudo-terminal while a read waits on it: Go keeps the file open until
// the read returns.)
func TestAHangupEndsEachArc(t *testing.T) {
	t.Parallel()
	for _, group := range []bool{false, true} {
		bob := dmtest.NewPerson(t, "bob")
		command, _, out := dmtest.OnTerminal(t, "watch", "-arc", bob.Arc.Program, "-home", bob.Arc.Home, "-state", bob.State, "-notify", "none")
		if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(out.String(), "watching") }) {
			t.Fatalf("the watch did not start:\n%s", out.String())
		}
		pid := command.Process.Pid
		if group {
			pid = -pid // arc-dm leads the session and the group of the terminal
		}
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = command.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(dmtest.WaitLimit):
			t.Fatalf("arc-dm went on after a hangup (to the group: %v)", group)
		}
		// arc-dm ends by its own handler, not by the signal: on Linux, a
		// signal that killed arc-dm would end its arc too, and hide a
		// missing handler.
		if command.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
			t.Errorf("a hangup killed arc-dm (to the group: %v), want it to end by its handler", group)
		}
		dmtest.NoArcLeft(t, bob.Arc.Home)
	}
}

// SIGTERM to the process of arc-dm alone ends each arc that it runs.
func TestSIGTERMEndsEachArc(t *testing.T) {
	t.Parallel()
	bob := dmtest.NewPerson(t, "bob")
	watch := bob.Command("watch", "-notify", "none")
	stderr := &dmtest.LockedBuffer{}
	watch.Stderr = stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	if err := watch.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
	// arc-dm ends by its own handler, not by the signal: on Linux, a
	// signal that killed arc-dm would end its arc too, and hide a missing
	// handler.
	if watch.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
		t.Error("SIGTERM killed arc-dm, want it to end by its handler")
	}
	dmtest.NoArcLeft(t, bob.Arc.Home)
}

// On Linux, an arc-dm that is killed takes each arc that it runs with it.
// macOS has no such signal: there, an arc of a killed arc-dm runs on.
func TestAKilledArcDmLeavesNoArcOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux kills a child when its parent dies")
	}
	t.Parallel()
	bob := dmtest.NewPerson(t, "bob")
	watch := bob.Command("watch", "-notify", "none")
	stderr := &dmtest.LockedBuffer{}
	watch.Stderr = stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	_ = watch.Process.Kill()
	_ = watch.Wait()
	dmtest.NoArcLeft(t, bob.Arc.Home)
}

// A read of the outbox while the watch runs takes at most 10 s. On an arc
// that hangs, the watch warns, and shows the reply that waits with no
// quote line. When a read works again, a later reply gets its quote.
func TestAWatchReadTimesOut(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "hello")
	bob.Sync()
	dir := t.TempDir()
	count, wrapper := filepath.Join(dir, "count"), filepath.Join(dir, "arc")
	// The second read of the outbox, the first timed read, hangs.
	sleep := fmt.Sprintf("%d.%d", 120+os.Getpid()%30, time.Now().Nanosecond()%1000)
	script := "#!/bin/sh\ncase " + `"$*"` + " in *" + `"message outbox"` + "*) echo x >> " + count + "; [ $(wc -l < " + count + ") -eq 2 ] && exec sleep " + sleep + ";; esac\nexec " + bob.Arc.Program + " " + `"$@"` + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exec.Command("pkill", "-f", "sleep "+sleep).Run() }()
	watch := exec.Command(dmtest.Program(), "watch", "-arc", wrapper, "-home", bob.Arc.Home, "-state", bob.State, "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	bob.DM("send", ada.Name, "bring the map")
	ada.Sync()
	short := dmtest.ShortID(t, ada.DM("open", bob.Name), "bring the map")
	ada.DM("reply", short, "yes")
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stdout.String(), "\n  yes\n") }) {
		t.Fatalf("the watch printed:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "\u21b3") {
		t.Errorf("the reply of the hung read showed a quote line:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "the read of the outbox took more than") {
		t.Errorf("the watch did not warn of the hung read:\n%s", stderr.String())
	}
	ada.DM("reply", short, "and the torch")
	want := "\n\u21b3 " + short + "  you: bring the map\n  and the torch\n"
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stdout.String(), want) }) {
		t.Fatalf("the watch printed:\n%s\nwant\n%s", stdout.String(), want)
	}
	_ = watch.Process.Signal(os.Interrupt)
	_ = watch.Wait()
}

// An outbox that always hangs does not hide replies past one read: after
// its 10 s, the read shows the held reply, with no quote line, and not as
// "(not on this device)": its parent is a message of this citizen.
func TestAnAlwaysHungOutboxShowsRepliesPlain(t *testing.T) {
	t.Parallel()
	ada, bob := dmtest.NewPerson(t, "ada"), dmtest.NewPerson(t, "bob")
	ada.DM("send", bob.Key, "hello")
	bob.Sync()
	dir := t.TempDir()
	count, wrapper := filepath.Join(dir, "count"), filepath.Join(dir, "arc")
	// Each read of the outbox after the first hangs.
	sleep := fmt.Sprintf("%d.%d", 200+os.Getpid()%30, time.Now().Nanosecond()%1000)
	script := "#!/bin/sh\ncase " + `"$*"` + " in *" + `"message outbox"` + "*) echo x >> " + count + "; [ $(wc -l < " + count + ") -ge 2 ] && exec sleep " + sleep + ";; esac\nexec " + bob.Arc.Program + " " + `"$@"` + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exec.Command("pkill", "-f", "sleep "+sleep).Run() }()
	watch := exec.Command(dmtest.Program(), "watch", "-arc", wrapper, "-home", bob.Arc.Home, "-state", bob.State, "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	bob.DM("send", ada.Name, "bring the map")
	ada.Sync()
	short := dmtest.ShortID(t, ada.DM("open", bob.Name), "bring the map")
	ada.DM("reply", short, "yes")
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stdout.String(), "\n  yes\n") }) {
		t.Fatalf("the watch printed:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if out := stdout.String(); strings.Contains(out, "\u21b3") || strings.Contains(out, "not on this device") {
		t.Errorf("the watch printed a quote line after a read that took too long:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "took more than") {
		t.Errorf("the watch did not warn of the hung read:\n%s", stderr.String())
	}
	_ = watch.Process.Signal(os.Interrupt)
	_ = watch.Wait()
}

// The ids that a watch shows as missing are saved when the watch ends,
// so a reply after it refuses them: here the watch shows the id after its
// timed read, and no message comes after that to save it sooner.
func TestAWatchSavesShownIdsAtItsEnd(t *testing.T) {
	t.Parallel()
	mallory, bob := dmtest.NewPerson(t, "mallory"), dmtest.NewPerson(t, "bob")
	mallory.DM("send", bob.Key, "hello")
	bob.Sync()
	watch := bob.Command("watch", "-notify", "none")
	stdout, stderr := &dmtest.LockedBuffer{}, &dmtest.LockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}
	parent := dmtest.FakeID(9)
	mallory.ArcSend(bob.Key, "see above", "--arctest-tag", "e,"+parent)
	if !arcprogramtest.WaitUntil(dmtest.WaitLimit, func() bool { return strings.Contains(stdout.String(), parent[:12]+"  (not on this device)") }) {
		t.Fatalf("the watch printed:\n%s", stdout.String())
	}
	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = watch.Wait()
	if out := bob.Refused("reply", parent[:12], "yes"); !strings.Contains(out, "was not on this device when arc-dm showed it") {
		t.Errorf("reply to the id that the watch showed as missing: %s", out)
	}
}
