package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gezibash/arc/runtime/homedir"
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
			dir, err := homedir.Abs(user, given.dir)
			return dir, given.why, err
		}
	}
	dir, err := homedir.Default(user)
	if err != nil {
		return "", "", err
	}
	old := filepath.Join(user, ".config", "arc")
	if warning := homedir.TwoHomes(user, dir); warning != "" {
		if _, warned := homeWarned.LoadOrStore(command.Root(), true); !warned {
			// A path can hold any character, such as a newline in HOME.
			fmt.Fprintln(command.ErrOrStderr(), "arc: warning: "+terminal.EscapeLine(warning))
		}
	}
	if dir == old {
		return dir, homedir.Older, nil
	}
	return dir, "~/.arc", nil
}

func flagString(command *cobra.Command, name string) string {
	value, _ := command.Flags().GetString(name)
	return value
}

// homeWarned holds each run of arc, by its root command, that warned of
// two homes already. arc finds its home more than once in one command.
var homeWarned sync.Map

// home is the directory of the identity that the command uses. It holds the
// key, the relay list, the installs and the store.
func home(command *cobra.Command) (string, error) {
	dir, _, err := chosen(command)
	return dir, err
}

func keyPath(dir string) string { return filepath.Join(dir, "key") }
