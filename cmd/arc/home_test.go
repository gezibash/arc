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
		t.Fatalf("the home is %s (%v), want %s", got, err, old)
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

// runDefaultErr is runDefault, and it also returns the standard error.
func runDefaultErr(t *testing.T, args ...string) (string, string, error) {
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

	if _, errs, err := runDefaultErr(t, "whoami"); err != nil || errs != "" {
		t.Fatalf("with one home, arc whoami: %v, warned %q", err, errs)
	}
	identity(t, old)
	for _, args := range [][]string{{"whoami"}, {"keys", "list"}, {"contacts"}} {
		_, errs, err := runDefaultErr(t, args...)
		if err != nil {
			t.Fatalf("arc %s: %v", strings.Join(args, " "), err)
		}
		if strings.Count(errs, "\n") != 1 || !strings.Contains(errs, dotArc) || !strings.Contains(errs, old) {
			t.Errorf("arc %s did not warn once, with both homes: %q", strings.Join(args, " "), errs)
		}
	}
	t.Setenv("ARC_HOME", dotArc)
	if _, errs, err := runDefaultErr(t, "whoami"); err != nil || errs != "" {
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

	out, errs, err := runDefaultErr(t, "whoami")
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
		_, _, err := runDefaultErr(t, args...)
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
	_, _, err := runDefaultErr(t, "whoami")
	if err == nil || strings.Contains(err.Error(), "keys gen") || !strings.Contains(err.Error(), path) {
		t.Fatalf("arc whoami: %v, want an error that names %s", err, path)
	}
}
