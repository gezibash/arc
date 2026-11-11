// Package homedir finds the home of arc: the folder that holds the
// identities of this machine, and the files of a relay that it runs.
package homedir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Abs is a home that --home or ARC_HOME gives, as a clean absolute
// path. A leading ~ is the user's home, as a shell expands it: bash does
// not expand it after =, as in --home=~/arc. A symlink keeps its own name.
func Abs(user, dir string) (string, error) {
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

// TwoHomes is the warning when both default homes exist, which says the
// home that arc uses and why, or "" when one home exists. arc writes
// nothing to the other home.
func TwoHomes(user, dir string) string {
	home := filepath.Join(user, ".arc")
	old := filepath.Join(user, ".config", "arc")
	switch {
	case dir == home && HoldsIdentity(home) && HoldsIdentity(old):
		return fmt.Sprintf("both %s and %s hold an identity. arc uses %s, because it holds an identity, and an older arc or arc-dm uses %s. Upgrade each arc and arc-dm on this machine, or set ARC_HOME.", home, old, home, old)
	case dir == old && HoldsIdentity(old):
		// Anything at ~/.arc: entries of arc, or a link that leads to no
		// home, as a dangling link or a link to ".".
		if _, err := os.Lstat(home); err == nil {
			// A link that leads to no home is not a second home.
			if holds, _ := Holds(home, "~/.arc"); !holds {
				return fmt.Sprintf("%s is a link that leads to no home of arc. arc uses %s, and reads and writes nothing through %s. Remove the link, or point it at a home, or set ARC_HOME.", home, old, home)
			}
			return fmt.Sprintf("both %s and %s exist. arc uses %s, because %s holds no identity, and reads and writes nothing in %s. Set ARC_HOME to choose the home.", home, old, old, home, home)
		}
	}
	return ""
}

// Names are the entries that arc keeps at the top of its home. An app
// that keeps data in ~/.arc/<app> never uses one of these names, and
// arc install refuses them.
func Names() []string {
	return []string{"citizens", "default", "device", "relay.db", "wake", "wake.toml"}
}

// Default is ~/.arc. A home that an older arc made in ~/.config/arc
// stays in use, until ~/.arc holds an entry of arc. arc moves no file, and
// it never looks for a home in the working directory: a cloned repository
// can hold a .arc with the key of someone else. A home that arc cannot
// read stops arc: arc never uses the next home in its place.
func Default(user string) (string, error) {
	home := filepath.Join(user, ".arc")
	old := filepath.Join(user, ".config", "arc")
	holds, err := Holds(home, "~/.arc")
	if err != nil {
		return home, err
	}
	if holds {
		// A ~/.arc with entries but no identity, such as a lone wake.toml,
		// does not hide the identity of an older home (#408). An older home
		// that arc cannot read stops arc: else keys gen would make a second
		// identity in ~/.arc, which then hides the first.
		if mine, err := IdentityIn(home, "~/.arc"); err != nil || mine {
			return home, err
		}
		older, err := IdentityIn(old, Older)
		if err != nil {
			return old, err
		}
		if older {
			return old, nil
		}
		return home, nil
	}
	if holds, err = Holds(old, Older); err != nil || holds {
		return old, err
	}
	return home, nil
}

// Older is the rule that chose ~/.config/arc.
const Older = "~/.config/arc, the home of an older arc"

// Holds says whether dir holds an entry of arc. Only an entry that is
// not there is missing: a folder that arc cannot read is an error. why is
// the rule that would choose dir.
func Holds(dir, why string) (bool, error) {
	for _, name := range Names() {
		_, err := os.Lstat(filepath.Join(dir, name))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, Unreadable(dir, why, err)
		}
	}
	return false, nil
}

func Unreadable(dir, why string, err error) error {
	fix := "Fix its permissions"
	switch {
	case errors.Is(err, syscall.ENOTDIR):
		fix = "It is not a folder: move it away"
	case errors.Is(err, syscall.ELOOP):
		fix = "It is a loop of symbolic links: remove the link"
	}
	return fmt.Errorf("cannot read the home %s, chosen by %s, and arc uses no other home in its place: %w. %s, or give --home or ARC_HOME", dir, why, err, fix)
}

// UnreadableDefault is the error of a default file that arc cannot read,
// or of its home, when the home is what arc cannot read.
func UnreadableDefault(root, why string, err error) error {
	if _, dirErr := os.ReadDir(root); dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
		return Unreadable(root, why, dirErr)
	}
	return fmt.Errorf("cannot read the default identity in %s, chosen by %s: %w. Fix the permissions of %s, or give --key or ARC_KEY", root, why, err, DefaultPath(root))
}

func UnreadableIdentities(root, why string, err error) error {
	if _, dirErr := os.ReadDir(root); dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
		return Unreadable(root, why, dirErr)
	}
	return fmt.Errorf("cannot read the identities in %s, chosen by %s: %w. Fix the permissions of that folder", CitizensDir(root), why, err)
}

// IdentityIn says whether dir holds an identity of arc. Only a folder of
// identities that is not there is missing: one that arc cannot read is an
// error. why is the rule that would choose dir.
func IdentityIn(dir, why string) (bool, error) {
	entries, err := os.ReadDir(CitizensDir(dir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, UnreadableIdentities(dir, why, err)
	}
	return len(entries) > 0, nil
}

// UnwritableIdentities is the error of a folder of identities that arc
// can read but cannot add to.
func UnwritableIdentities(root, why string, err error) error {
	return fmt.Errorf("cannot add an identity to %s, chosen by %s: %w. Fix the permissions of that folder", CitizensDir(root), why, err)
}

// HoldsIdentity says whether dir holds an identity of arc.
func HoldsIdentity(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "citizens"))
	return err == nil && len(entries) > 0
}

func CitizensDir(root string) string { return filepath.Join(root, "citizens") }
func DefaultPath(root string) string { return filepath.Join(root, "default") }
