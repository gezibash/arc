package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/gezibash/arc/sdk/terminal"
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
		if strings.Count(errs, "\n") != 1 || !strings.Contains(errs, old) || !strings.Contains(errs, "arc uses "+dotArc+", because it holds an identity") {
			t.Errorf("arc %s did not warn once, with both homes and the one in use: %q", strings.Join(args, " "), errs)
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

// A ~/.arc that holds only a wake.toml does not hide the identity of an
// older home (#408). arc warns that it uses the older home and why, and
// writes nothing to ~/.arc.
func TestALoneWakeFileDoesNotHideTheOlderIdentity(t *testing.T) {
	home := user(t)
	dotArc, old := filepath.Join(home, ".arc"), filepath.Join(home, ".config", "arc")
	older := identity(t, old)
	if err := os.MkdirAll(dotArc, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotArc, "wake.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, err := runErr(t, "whoami")
	if err != nil || !strings.Contains(out, older) {
		t.Fatalf("arc whoami: %v, want the identity %s of ~/.config/arc in %q", err, older, out)
	}
	if strings.Count(errs, "\n") != 1 || !strings.Contains(errs, "arc uses "+old+", because "+dotArc+" holds no identity") {
		t.Errorf("arc whoami did not warn once with the home in use and why: %q", errs)
	}
	if _, _, err := runErr(t, "keys", "gen"); err != nil {
		t.Fatalf("arc keys gen: %v", err)
	}
	entries, err := os.ReadDir(dotArc)
	if err != nil || len(entries) != 1 || entries[0].Name() != "wake.toml" {
		t.Errorf("arc wrote to %s, which it does not use: %v, %v", dotArc, entries, err)
	}
}

// Each home, default file and folder of identities that arc cannot read
// has one error, which names the folder and the rule that chose it, and
// what fixes it (#408, #411).
func TestEachUnreadablePartOfAHomeHasOneError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder of mode 000")
	}
	home := user(t)
	dot := filepath.Join(home, ".arc")
	identity(t, dot)
	lock := func(path string) {
		t.Helper()
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
	}
	check := func(what string, want []string, args ...string) {
		t.Helper()
		_, _, err := runErr(t, args...)
		if err == nil {
			t.Fatalf("%s: arc %v worked", what, args)
		}
		for _, part := range want {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("%s: arc %v said %q, want %q in it", what, args, err, part)
			}
		}
	}

	// (c): only the default file is unreadable; the home is readable.
	lock(filepath.Join(dot, "default"))
	for _, args := range [][]string{{"whoami"}, {"keys", "gen"}, {"keys", "list"}, {"keys", "ls"}} {
		check("an unreadable default", []string{"cannot read the default identity in " + dot + ", chosen by ~/.arc", "Fix the permissions of " + filepath.Join(dot, "default")}, args...)
	}
	_ = os.Chmod(filepath.Join(dot, "default"), 0o600)

	// The folder of identities.
	lock(filepath.Join(dot, "citizens"))
	check("unreadable identities", []string{"cannot read the identities in " + filepath.Join(dot, "citizens") + ", chosen by ~/.arc"}, "keys", "list")
	check("unreadable identities", []string{"cannot read the identities in " + filepath.Join(dot, "citizens"), "chosen by ~/.arc"}, "whoami")
	check("unreadable identities", []string{"cannot read the identities in " + filepath.Join(dot, "citizens"), "chosen by ~/.arc"}, "keys", "gen")
	// A folder of identities that arc reads but cannot add to.
	if err := os.Chmod(filepath.Join(dot, "citizens"), 0o500); err != nil {
		t.Fatal(err)
	}
	check("read-only identities", []string{"cannot add an identity to " + filepath.Join(dot, "citizens") + ", chosen by ~/.arc", "Fix the permissions of that folder"}, "keys", "gen")
	_ = os.Chmod(filepath.Join(dot, "citizens"), 0o700)

	// (a) and (b): the home itself, chosen by default and by --home.
	flag := t.TempDir()
	identity(t, flag)
	lock(flag)
	check("an unreadable --home", []string{"cannot read the home " + flag + ", chosen by --home", "Fix its permissions"}, "--home", flag, "whoami")
	lock(dot)
	check("an unreadable ~/.arc", []string{"cannot read the home " + dot + ", chosen by ~/.arc", "Fix its permissions"}, "whoami")
}

// --home ~root/x names the home of another user: arc refuses it, and makes
// no folder named ~root (#411).
func TestAHomeOfAnotherUserIsRefused(t *testing.T) {
	user(t)
	work := t.TempDir()
	t.Chdir(work)
	for _, dir := range []string{"~root/x", "~nosuchuser/x", "~+/x", "~-/x"} {
		if _, _, err := runErr(t, "--home", dir, "keys", "gen"); err == nil || !strings.Contains(err.Error(), "arc expands only ~ and ~/") {
			t.Errorf("--home %s gave %v", dir, err)
		}
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("arc made %v in the working directory", entries)
	}
}

// keys ls lists the identities, as keys list does (#408).
func TestKeysLsLists(t *testing.T) {
	home := user(t)
	name := identity(t, filepath.Join(home, ".arc"))
	if out, err := runDefault(t, "keys", "ls"); err != nil || !strings.Contains(out, name) {
		t.Errorf("keys ls printed %q, %v; want %s", out, err, name)
	}
}

// A lone ~/.arc/wake.toml gives way to an older home only when arc can read
// that home. An unreadable ~/.config/arc, or its folder of identities, stops
// arc, and keys gen writes no second identity in ~/.arc, which would hide
// the first once the permissions are fixed.
func TestAnUnreadableOlderHomeBesideALoneWakeFileStopsArc(t *testing.T) {
	for _, part := range []string{"", "citizens"} {
		home := user(t)
		dotArc, old := filepath.Join(home, ".arc"), filepath.Join(home, ".config", "arc")
		identity(t, old)
		if err := os.MkdirAll(dotArc, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dotArc, "wake.toml"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		lock(t, filepath.Join(old, part))
		want := "cannot read the home " + old
		if part != "" {
			want = "cannot read the identities in " + filepath.Join(old, "citizens")
		}
		for _, args := range [][]string{{"whoami"}, {"keys", "gen"}} {
			if _, _, err := runErr(t, args...); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("with %s locked, arc %s gave %v, want %q", filepath.Join(old, part), strings.Join(args, " "), err, want)
			}
		}
		if entries, err := os.ReadDir(dotArc); err != nil || len(entries) != 1 {
			t.Errorf("with %s locked, arc wrote to %s: %v, %v", filepath.Join(old, part), dotArc, entries, err)
		}
	}
}

// A ~/.arc that is a plain file, or a loop of symbolic links, stops arc
// with an error that says so. arc never uses the identity of ~/.config/arc
// in its place, and writes nothing.
func TestADotArcThatIsNoFolderStopsArc(t *testing.T) {
	for _, shape := range []string{"file", "loop"} {
		home := user(t)
		dotArc, old := filepath.Join(home, ".arc"), filepath.Join(home, ".config", "arc")
		other := identity(t, old)
		var err error
		want := "It is not a folder"
		if shape == "file" {
			err = os.WriteFile(dotArc, nil, 0o600)
		} else {
			err, want = os.Symlink(dotArc, dotArc), "It is a loop of symbolic links"
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"whoami"}, {"keys", "gen"}} {
			out, _, err := runErr(t, args...)
			if err == nil || strings.Contains(out, other) || !strings.Contains(err.Error(), "cannot read the home "+dotArc) || !strings.Contains(err.Error(), want) {
				t.Errorf("a ~/.arc that is a %s: arc %s printed %q, %v; want %q", shape, strings.Join(args, " "), out, err, want)
			}
		}
		if entries, _ := os.ReadDir(citizensDir(old)); len(entries) != 1 {
			t.Errorf("a ~/.arc that is a %s: ~/.config/arc holds %d identities, want 1", shape, len(entries))
		}
	}
}

// hostileHome is a HOME whose name holds a newline, a forged line of arc
// and control characters.
func hostileHome(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "h\narc: fake forged\x1b[2J\r\x07")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("ARC_HOME", "")
	t.Setenv("ARC_KEY", "")
	return dir
}

// Both warnings of two homes escape each path on one line: a HOME with a
// newline and control characters forges no line of arc (#431).
func TestTheWarningsOfTwoHomesAreOneEscapedLine(t *testing.T) {
	for _, lone := range []bool{false, true} {
		home := hostileHome(t)
		identity(t, filepath.Join(home, ".config", "arc"))
		if lone {
			if err := os.MkdirAll(filepath.Join(home, ".arc"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".arc", "wake.toml"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			identity(t, filepath.Join(home, ".arc"))
		}
		_, errs, err := runErr(t, "whoami")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(errs, "\n") != 1 || !strings.HasPrefix(errs, "arc: warning: ") || strings.ContainsAny(errs, "\x1b\r\x07") || strings.Contains(errs, "\narc: fake") ||
			!strings.Contains(errs, `h\narc: fake forged\x1b[2J\x0d\x07`) {
			t.Errorf("with a lone ~/.arc %v, the warning is %q, want one escaped line", lone, errs)
		}
	}
}

// A ~/.arc that is a dangling link, or a link to ".", gives way to the
// identity of ~/.config/arc, and warns, as a lone wake.toml does.
func TestALinkThatLeadsToNoHomeWarns(t *testing.T) {
	for _, target := range []string{"nowhere", "."} {
		home := user(t)
		old := filepath.Join(home, ".config", "arc")
		older := identity(t, old)
		if err := os.Symlink(target, filepath.Join(home, ".arc")); err != nil {
			t.Fatal(err)
		}
		out, errs, err := runErr(t, "whoami")
		if err != nil || !strings.Contains(out, older) {
			t.Fatalf("with ~/.arc -> %s, arc whoami: %v, %q", target, err, out)
		}
		if strings.Count(errs, "\n") != 1 || !strings.Contains(errs, "arc uses "+old+", because ") {
			t.Errorf("with ~/.arc -> %s, arc warned %q, want the home in use and why", target, errs)
		}
	}
}

// The refusal of a ~ home names the home escaped once (#431).
func TestTheRefusalOfATildeHomeIsEscapedOnce(t *testing.T) {
	user(t)
	_, _, err := runErr(t, "--home", "~x\ny/z", "whoami")
	if err == nil || !strings.Contains(err.Error(), "\"~x\ny/z\"") {
		t.Fatalf("the refusal is %v, want the home as given, for the edge to escape once", err)
	}
	if got := terminal.EscapeLine(err.Error()); !strings.Contains(got, `"~x\ny/z"`) {
		t.Errorf("the escaped refusal is %q, want \\n once", got)
	}
}

// forgedOrRaw says whether out holds a line that a hostile name forged, or
// a raw control character.
func forgedOrRaw(out string) bool {
	return strings.Contains("\n"+out, "\narc: fake") || strings.ContainsFunc(out, func(r rune) bool { return r != '\n' && unicode.IsControl(r) })
}

// No name or path that a user or the file system controls forges a line of
// arc, or reaches the terminal raw (#441): a HOME with a newline, an
// identity folder with one, and a relay URL with one, which arc refuses.
func TestNoHostileNameForgesALine(t *testing.T) {
	home := hostileHome(t)
	dot := filepath.Join(home, ".arc")
	for _, args := range [][]string{{"keys", "gen"}, {"whoami"}, {"keys", "list"}} {
		out, errs, err := runErr(t, args...)
		if err != nil {
			t.Fatalf("arc %s: %v", strings.Join(args, " "), err)
		}
		if forgedOrRaw(out + errs) {
			t.Errorf("arc %s with a hostile HOME printed %q", strings.Join(args, " "), out+errs)
		}
	}
	hostile := filepath.Join(citizensDir(dot), "x\narc: fake forged\x1b[2J")
	if err := os.MkdirAll(hostile, 0o700); err != nil {
		t.Fatal(err)
	}
	out, errs, err := runErr(t, "keys", "list")
	if err != nil || forgedOrRaw(out+errs) || !strings.Contains(out, `x\narc: fake forged\x1b[2J`) {
		t.Errorf("arc keys list with a hostile identity folder printed %q, %v", out+errs, err)
	}
	if _, _, err := runErr(t, "relay", "add", "ws://a.example\narc: fake forged"); err == nil || !strings.Contains(err.Error(), "no space and no control character") {
		t.Errorf("arc relay add of a URL with a newline gave %v, want a refusal", err)
	}
	if relays, _ := readRelays(dot); len(relays) != 0 {
		t.Errorf("the refused URL left the relays %q", relays)
	}
}
