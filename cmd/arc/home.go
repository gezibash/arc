package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// rootDir is the directory of arc. It holds one directory for each identity,
// and the files of a relay that this machine runs.
func rootDir(command *cobra.Command) (string, error) {
	dir, _, err := rootSource(command)
	return dir, err
}

// rootSource is rootDir, and the rule that chose it: --home, ARC_HOME,
// ~/.arc, or the home of an older arc.
func rootSource(command *cobra.Command) (string, string, error) {
	user, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	for _, given := range []struct{ dir, why string }{
		{flagString(command, "home"), "--home"},
		{os.Getenv("ARC_HOME"), "ARC_HOME"},
	} {
		if given.dir != "" {
			dir, err := absHome(user, given.dir)
			return dir, given.why, err
		}
	}
	dir, err := defaultRoot(user)
	if err != nil {
		return "", "", err
	}
	old := filepath.Join(user, ".config", "arc")
	if warning := twoHomes(user, dir); warning != "" {
		if _, warned := homeWarned.LoadOrStore(command.Root(), true); !warned {
			// A path can hold any character, such as a newline in HOME.
			fmt.Fprintln(command.ErrOrStderr(), "arc: warning: "+terminal.EscapeLine(warning))
		}
	}
	if dir == old {
		return dir, olderHome, nil
	}
	return dir, "~/.arc", nil
}

func flagString(command *cobra.Command, name string) string {
	value, _ := command.Flags().GetString(name)
	return value
}

// absHome is a home that --home or ARC_HOME gives, as a clean absolute
// path. A leading ~ is the user's home, as a shell expands it: bash does
// not expand it after =, as in --home=~/arc. A symlink keeps its own name.
func absHome(user, dir string) (string, error) {
	if dir == "~" {
		dir = user
	} else if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		dir = filepath.Join(user, rest)
	} else if strings.HasPrefix(dir, "~") {
		// A shell expands ~root to the home of a user, and ~+ to the working
		// directory, so a folder of that name here would be one rm -rf away
		// from the folder that it names there (#411).
		return "", fmt.Errorf("the home \"%s\" starts with a ~ that a shell expands to another folder: arc expands only ~ and ~/; give the whole path", dir)
	}
	return filepath.Abs(dir)
}

// twoHomes is the warning when both default homes exist, which says the
// home that arc uses and why, or "" when one home exists. arc writes
// nothing to the other home.
func twoHomes(user, dir string) string {
	home := filepath.Join(user, ".arc")
	old := filepath.Join(user, ".config", "arc")
	switch {
	case dir == home && holdsIdentity(home) && holdsIdentity(old):
		return fmt.Sprintf("both %s and %s hold an identity. arc uses %s, because it holds an identity, and an older arc or arc-dm uses %s. Upgrade each arc and arc-dm on this machine, or set ARC_HOME.", home, old, home, old)
	case dir == old && holdsIdentity(old):
		// Anything at ~/.arc: entries of arc, or a link that leads to no
		// home, as a dangling link or a link to ".".
		if _, err := os.Lstat(home); err == nil {
			// A link that leads to no home is not a second home.
			if holds, _ := holdsHome(home, "~/.arc"); !holds {
				return fmt.Sprintf("%s is a link that leads to no home of arc. arc uses %s, and reads and writes nothing through %s. Remove the link, or point it at a home, or set ARC_HOME.", home, old, home)
			}
			return fmt.Sprintf("both %s and %s exist. arc uses %s, because %s holds no identity, and reads and writes nothing in %s. Set ARC_HOME to choose the home.", home, old, old, home, home)
		}
	}
	return ""
}

// homeWarned holds each run of arc, by its root command, that warned of
// two homes already. arc finds its home more than once in one command.
var homeWarned sync.Map

// homeNames are the entries that arc keeps at the top of its home. An app
// that keeps data in ~/.arc/<app> never uses one of these names, and
// arc install refuses them.
var homeNames = []string{"citizens", "default", "device", "relay.db", "wake", "wake.toml"}

// defaultRoot is ~/.arc. A home that an older arc made in ~/.config/arc
// stays in use, until ~/.arc holds an entry of arc. arc moves no file, and
// it never looks for a home in the working directory: a cloned repository
// can hold a .arc with the key of someone else. A home that arc cannot
// read stops arc: arc never uses the next home in its place.
func defaultRoot(user string) (string, error) {
	home := filepath.Join(user, ".arc")
	old := filepath.Join(user, ".config", "arc")
	holds, err := holdsHome(home, "~/.arc")
	if err != nil {
		return home, err
	}
	if holds {
		// A ~/.arc with entries but no identity, such as a lone wake.toml,
		// does not hide the identity of an older home (#408). An older home
		// that arc cannot read stops arc: else keys gen would make a second
		// identity in ~/.arc, which then hides the first.
		if mine, err := identityIn(home, "~/.arc"); err != nil || mine {
			return home, err
		}
		older, err := identityIn(old, olderHome)
		if err != nil {
			return old, err
		}
		if older {
			return old, nil
		}
		return home, nil
	}
	if holds, err = holdsHome(old, olderHome); err != nil || holds {
		return old, err
	}
	return home, nil
}

// olderHome is the rule that chose ~/.config/arc.
const olderHome = "~/.config/arc, the home of an older arc"

// holdsHome says whether dir holds an entry of arc. Only an entry that is
// not there is missing: a folder that arc cannot read is an error. why is
// the rule that would choose dir.
func holdsHome(dir, why string) (bool, error) {
	for _, name := range homeNames {
		_, err := os.Lstat(filepath.Join(dir, name))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, unreadableHome(dir, why, err)
		}
	}
	return false, nil
}

func unreadableHome(dir, why string, err error) error {
	fix := "Fix its permissions"
	switch {
	case errors.Is(err, syscall.ENOTDIR):
		fix = "It is not a folder: move it away"
	case errors.Is(err, syscall.ELOOP):
		fix = "It is a loop of symbolic links: remove the link"
	}
	return fmt.Errorf("cannot read the home %s, chosen by %s, and arc uses no other home in its place: %w. %s, or give --home or ARC_HOME", dir, why, err, fix)
}

// unreadableDefault is the error of a default file that arc cannot read,
// or of its home, when the home is what arc cannot read.
func unreadableDefault(root, why string, err error) error {
	if _, dirErr := os.ReadDir(root); dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
		return unreadableHome(root, why, dirErr)
	}
	return fmt.Errorf("cannot read the default identity in %s, chosen by %s: %w. Fix the permissions of %s, or give --key or ARC_KEY", root, why, err, defaultPath(root))
}

func unreadableIdentities(root, why string, err error) error {
	if _, dirErr := os.ReadDir(root); dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
		return unreadableHome(root, why, dirErr)
	}
	return fmt.Errorf("cannot read the identities in %s, chosen by %s: %w. Fix the permissions of that folder", citizensDir(root), why, err)
}

// identityIn says whether dir holds an identity of arc. Only a folder of
// identities that is not there is missing: one that arc cannot read is an
// error. why is the rule that would choose dir.
func identityIn(dir, why string) (bool, error) {
	entries, err := os.ReadDir(citizensDir(dir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, unreadableIdentities(dir, why, err)
	}
	return len(entries) > 0, nil
}

// unwritableIdentities is the error of a folder of identities that arc
// can read but cannot add to.
func unwritableIdentities(root, why string, err error) error {
	return fmt.Errorf("cannot add an identity to %s, chosen by %s: %w. Fix the permissions of that folder", citizensDir(root), why, err)
}

// holdsIdentity says whether dir holds an identity of arc.
func holdsIdentity(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "citizens"))
	return err == nil && len(entries) > 0
}

// home is the directory of the identity that the command uses. It holds the
// key, the relay list, the installs and the store.
func home(command *cobra.Command) (string, error) {
	dir, _, err := chosen(command)
	return dir, err
}

func keyPath(dir string) string { return filepath.Join(dir, "key") }
