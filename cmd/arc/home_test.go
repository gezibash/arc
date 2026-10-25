package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// user makes a fresh user home, with no ARC_HOME and no ARC_KEY.
func user(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("ARC_HOME", "")
	t.Setenv("ARC_KEY", "")
	return dir
}

// runDefault runs arc with no --home, so arc picks its home.
func runDefault(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	command := root()
	command.SetIn(strings.NewReader(""))
	command.SetOut(&out)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

// identity makes an identity in dir, and returns its name.
func identity(t *testing.T, dir string) string {
	t.Helper()
	return strings.Fields(ok(t, dir, "", "keys", "gen"))[0]
}

// whoami is the name of the identity that arc picks with no --home.
func whoami(t *testing.T) string {
	t.Helper()
	out, err := runDefault(t, "whoami")
	if err != nil {
		t.Fatalf("arc whoami: %v\n%s", err, out)
	}
	return strings.Fields(out)[0]
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A machine that an older arc set up keeps its identity in ~/.config/arc,
// and ~/.arc holds only the data of apps. The newer arc uses the older home
// and moves nothing.
func TestAnOlderHomeStillWorks(t *testing.T) {
	home := user(t)
	old := filepath.Join(home, ".config", "arc")
	name := identity(t, old)
	for _, app := range []string{"dm", "exec", "journal"} {
		if err := os.MkdirAll(filepath.Join(home, ".arc", app), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if got := whoami(t); got != name {
		t.Fatalf("arc picked %s, want %s from ~/.config/arc", got, name)
	}
	if _, err := runDefault(t, "keys", "device"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range homeNames {
		if exists(filepath.Join(home, ".arc", entry)) {
			t.Errorf("arc wrote %s in ~/.arc, while its home is ~/.config/arc", entry)
		}
	}
	if !exists(filepath.Join(old, "device")) {
		t.Error("the device key is not in ~/.config/arc")
	}
}

// A user with no home of arc gets ~/.arc.
func TestANewHomeIsDotArc(t *testing.T) {
	home := user(t)
	out, err := runDefault(t, "keys", "gen")
	if err != nil {
		t.Fatalf("arc keys gen: %v\n%s", err, out)
	}
	if !exists(filepath.Join(home, ".arc", "citizens")) || !exists(filepath.Join(home, ".arc", "default")) {
		t.Fatal("the new identity is not in ~/.arc")
	}
	if exists(filepath.Join(home, ".config", "arc")) {
		t.Fatal("arc made ~/.config/arc for a new identity")
	}
}

// When both homes hold an identity, ~/.arc wins.
func TestDotArcWinsOverTheOlderHome(t *testing.T) {
	home := user(t)
	identity(t, filepath.Join(home, ".config", "arc"))
	name := identity(t, filepath.Join(home, ".arc"))
	if got := whoami(t); got != name {
		t.Fatalf("arc picked %s, want %s from ~/.arc", got, name)
	}
}

// A relay that an older arc ran keeps its events in ~/.config/arc/relay.db,
// with no identity beside them. The newer arc keeps that home.
func TestARelayKeepsItsOlderHome(t *testing.T) {
	home := user(t)
	old := filepath.Join(home, ".config", "arc")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "relay.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := defaultRoot(home); err != nil || got != old {
		t.Fatalf("the home is %s, want %s", got, old)
	}
}

// --home wins over ARC_HOME, and ARC_HOME wins over both default homes.
func TestTheFlagAndTheVariableWin(t *testing.T) {
	home := user(t)
	identity(t, filepath.Join(home, ".config", "arc"))
	identity(t, filepath.Join(home, ".arc"))
	variableHome, flag := t.TempDir(), t.TempDir()
	variable, flagged := identity(t, variableHome), identity(t, flag)

	t.Setenv("ARC_HOME", variableHome)
	if got := whoami(t); got != variable {
		t.Fatalf("with ARC_HOME, arc picked %s, want %s", got, variable)
	}
	out, err := runDefault(t, "--home", flag, "whoami")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(out)[0]; got != flagged {
		t.Fatalf("with --home and ARC_HOME, arc picked %s, want %s", got, flagged)
	}
}

// A cloned repository can hold a .arc with the key of someone else. arc
// never takes its home from the working directory or a parent of it.
func TestNoHomeFromTheWorkingDirectory(t *testing.T) {
	user(t)
	repo := t.TempDir()
	identity(t, filepath.Join(repo, ".arc"))
	nested := filepath.Join(repo, "src")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{repo, nested} {
		t.Chdir(dir)
		if out, err := runDefault(t, "whoami"); err == nil {
			t.Fatalf("in %s, arc used the .arc of the working directory: %s", dir, out)
		}
	}
}

// runErr runs arc with no --home, and returns its standard output and
// standard error.
func runErr(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errs strings.Builder
	command := root()
	command.SetIn(strings.NewReader(""))
	command.SetOut(&out)
	command.SetErr(&errs)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), errs.String(), err
}

// whoami names the home in use, and the rule that chose it (#406: nothing
// showed which home arc used).
func TestWhoamiShowsTheHomeInUse(t *testing.T) {
	home := user(t)
	older := filepath.Join(home, ".config", "arc")
	identity(t, older)
	check := func(want string, args ...string) {
		t.Helper()
		out, _, err := runErr(t, append(args, "whoami")...)
		if err != nil {
			t.Fatalf("arc whoami: %v\n%s", err, out)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 4 || lines[3] != want {
			t.Errorf("arc %v whoami printed %q, want the last line %q", args, out, want)
		}
	}
	check("home " + older + ", chosen by ~/.config/arc, the home of an older arc")

	identity(t, filepath.Join(home, ".arc"))
	check("home " + filepath.Join(home, ".arc") + ", chosen by ~/.arc")

	variable := t.TempDir()
	identity(t, variable)
	t.Setenv("ARC_HOME", variable)
	check("home " + variable + ", chosen by ARC_HOME")

	flag := t.TempDir()
	identity(t, flag)
	check("home "+flag+", chosen by --home", "--home", flag)
}

// keys gen and keys recover say where they wrote the identity, on the
// standard error, so the standard output keeps the name and the key (#406).
func TestKeysGenSaysWhereItWrote(t *testing.T) {
	home := user(t)
	out, errs, err := runErr(t, "keys", "gen")
	if err != nil {
		t.Fatalf("arc keys gen: %v\n%s", err, errs)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 {
		t.Errorf("keys gen printed %q, want the name and the key", out)
	}
	if want := "wrote the identity in " + filepath.Join(home, ".arc"); !strings.Contains(errs, want) {
		t.Errorf("keys gen said %q, want %q", errs, want)
	}
}

// homeLine is the last line of arc whoami: the home in use.
func homeLine(t *testing.T, args ...string) string {
	t.Helper()
	out, errs, err := runErr(t, append(args, "whoami")...)
	if err != nil {
		t.Fatalf("arc %v whoami: %v\n%s", args, err, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

// A home of --home or ARC_HOME that starts with ~ is in the user's home, as
// the shell would expand it; bash does not expand ~ after = (Otto's test of
// #410: --home=~/tgen made a folder named ~ in the working directory).
func TestAHomeWithATildeIsInTheUserHome(t *testing.T) {
	home := user(t)
	work := t.TempDir()
	t.Chdir(work)
	if _, errs, err := runErr(t, "--home=~/tgen", "keys", "gen"); err != nil {
		t.Fatalf("arc --home=~/tgen keys gen: %v\n%s", err, errs)
	} else if want := "wrote the identity in " + filepath.Join(home, "tgen"); !strings.Contains(errs, want) {
		t.Errorf("keys gen said %q, want %q", errs, want)
	}
	if exists(filepath.Join(work, "~")) {
		t.Error("arc made a folder named ~ in the working directory")
	}
	if got, want := homeLine(t, "--home=~/tgen"), "home "+filepath.Join(home, "tgen")+", chosen by --home"; got != want {
		t.Errorf("whoami says %q, want %q", got, want)
	}
	t.Setenv("ARC_HOME", "~/tgen")
	if got, want := homeLine(t), "home "+filepath.Join(home, "tgen")+", chosen by ARC_HOME"; got != want {
		t.Errorf("whoami says %q, want %q", got, want)
	}
}

// The home is printed as a clean absolute path. A symlink keeps its own name.
func TestTheHomeIsAnAbsoluteCleanPath(t *testing.T) {
	user(t)
	work := t.TempDir()
	t.Chdir(work)
	for _, dir := range []string{"relgen", "homes//./flagH", "../" + filepath.Base(work) + "/homes/x"} {
		identity(t, filepath.Join(work, filepath.Clean(dir)))
		want := "home " + filepath.Join(work, filepath.Clean(dir)) + ", chosen by --home"
		if strings.HasPrefix(dir, "..") {
			want = "home " + filepath.Join(work, "homes", "x") + ", chosen by --home"
		}
		if got := homeLine(t, "--home", dir); got != want {
			t.Errorf("--home %s: whoami says %q, want %q", dir, got, want)
		}
	}
	if err := os.Symlink(filepath.Join(work, "relgen"), filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	if got, want := homeLine(t, "--home", "link/"), "home "+filepath.Join(work, "link")+", chosen by --home"; got != want {
		t.Errorf("a symlinked home: whoami says %q, want %q", got, want)
	}
}

// When whoami finds no identity, it names the home and its rule. A home that
// it cannot read says so, and does not ask for a new identity.
func TestAFailedWhoamiNamesTheHome(t *testing.T) {
	home := user(t)
	_, _, err := runErr(t, "whoami")
	if err == nil || !strings.Contains(err.Error(), "no identity in "+filepath.Join(home, ".arc")+", chosen by ~/.arc") {
		t.Errorf("whoami in a fresh home gave %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder of mode 000")
	}
	locked := t.TempDir()
	identity(t, locked)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	_, _, err = runErr(t, "--home", locked, "whoami")
	if err == nil || !strings.Contains(err.Error(), "cannot read") || !strings.Contains(err.Error(), locked) || strings.Contains(err.Error(), "make one") {
		t.Errorf("whoami of a home that it cannot read gave %v", err)
	}
}

// lock makes dir unreadable until the test ends, as mode 000 or a home
// that root owns after sudo arc does.
func lock(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder of mode 000")
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// An older arc that does not see ~/.arc makes a second identity in
// ~/.config/arc. Each command that picks the default home warns in one line,
// and names both folders. With ARC_HOME, or with one home, no warning shows.
func TestTwoHomesWithAnIdentityWarn(t *testing.T) {
	home := user(t)
	dotArc, old := filepath.Join(home, ".arc"), filepath.Join(home, ".config", "arc")
	identity(t, dotArc)

	if _, errs, err := runErr(t, "whoami"); err != nil || errs != "" {
		t.Fatalf("with one home, arc whoami: %v, warned %q", err, errs)
	}
	identity(t, old)
	for _, args := range [][]string{{"whoami"}, {"keys", "list"}, {"contacts"}} {
		_, errs, err := runErr(t, args...)
		if err != nil {
			t.Fatalf("arc %s: %v", strings.Join(args, " "), err)
		}
		if strings.Count(errs, "\n") != 1 || !strings.Contains(errs, dotArc) || !strings.Contains(errs, old) {
			t.Errorf("arc %s did not warn once, with both homes: %q", strings.Join(args, " "), errs)
		}
	}
	t.Setenv("ARC_HOME", dotArc)
	if _, errs, err := runErr(t, "whoami"); err != nil || errs != "" {
		t.Fatalf("with ARC_HOME, arc whoami: %v, warned %q", err, errs)
	}
}

// A ~/.arc that arc cannot read stops arc. It never takes the identity of
// ~/.config/arc in its place.
func TestAnUnreadableDotArcStopsArc(t *testing.T) {
	home := user(t)
	dotArc, old := filepath.Join(home, ".arc"), filepath.Join(home, ".config", "arc")
	identity(t, dotArc)
	other := identity(t, old)
	lock(t, dotArc)

	out, errs, err := runErr(t, "whoami")
	if err == nil || strings.Contains(out, other) {
		t.Fatalf("arc whoami used the identity of ~/.config/arc: %q", out)
	}
	if !strings.Contains(err.Error(), "cannot read the home "+dotArc) {
		t.Fatalf("the error does not name %s: %v\n%s", dotArc, err, errs)
	}
}

// A ~/.config/arc that arc cannot read stops arc too: arc does not make a
// second identity in ~/.arc.
func TestAnUnreadableOlderHomeStopsArc(t *testing.T) {
	home := user(t)
	old := filepath.Join(home, ".config", "arc")
	identity(t, old)
	lock(t, old)

	for _, args := range [][]string{{"whoami"}, {"keys", "gen"}} {
		_, _, err := runErr(t, args...)
		if err == nil || !strings.Contains(err.Error(), "cannot read the home "+old) {
			t.Errorf("arc %s: %v, want an error that names %s", strings.Join(args, " "), err, old)
		}
	}
	if exists(filepath.Join(home, ".arc")) {
		t.Fatal("arc made ~/.arc in place of the unreadable ~/.config/arc")
	}
}

// A default file that arc cannot read is not a missing identity: arc says
// that it cannot read it, and does not tell the user to make one.
func TestAnUnreadableDefaultIsNoMissingIdentity(t *testing.T) {
	home := user(t)
	identity(t, filepath.Join(home, ".arc"))
	path := filepath.Join(home, ".arc", "default")
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, _, err := runErr(t, "whoami")
	if err == nil || strings.Contains(err.Error(), "keys gen") || !strings.Contains(err.Error(), path) {
		t.Fatalf("arc whoami: %v, want an error that names %s", err, path)
	}
}

// A default file that arc cannot read stops arc keys gen before it writes
// an identity. Else arc keeps a key whose seed nobody saw.
func TestAnUnreadableDefaultStopsKeysGen(t *testing.T) {
	home := user(t)
	identity(t, filepath.Join(home, ".arc"))
	path := filepath.Join(home, ".arc", "default")
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, _, err := runErr(t, "keys", "gen")
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("arc keys gen: %v, want an error that names %s", err, path)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".arc", "citizens"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("arc keys gen kept %d identities, want the 1 from before", len(entries))
	}
}
