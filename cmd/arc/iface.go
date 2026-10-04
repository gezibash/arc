package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/spf13/cobra"
)

// dispatch runs a command of an installed app: arc <name> <path...>.
// The root command does not parse flags, so the app reads its own;
// dispatch reads only a leading --home and --key.
func dispatch(command *cobra.Command, args []string) error {
	for len(args) > 0 {
		flag := ""
		for _, name := range []string{"home", "key"} {
			if args[0] == "--"+name || strings.HasPrefix(args[0], "--"+name+"=") {
				flag = name
			}
		}
		if flag == "" {
			break
		}
		value, ok := strings.CutPrefix(args[0], "--"+flag+"=")
		args = args[1:]
		if !ok {
			if len(args) == 0 {
				return fmt.Errorf("--%s needs a value", flag)
			}
			value, args = args[0], args[1:]
		}
		if err := command.Flags().Set(flag, value); err != nil {
			return err
		}
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return command.Help()
	}
	// arc update runs a new program with --version before it swaps it in.
	if args[0] == "--version" || args[0] == "-v" {
		fmt.Printf("arc %s\n", version)
		return nil
	}
	return runCapability(command, args[0], args[1:])
}

func runCapability(command *cobra.Command, name string, words []string) error {
	installs, err := installsOf(command)
	if err != nil {
		return err
	}

	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()

	in, err := sess.Installed(command.Context(), installs, name)
	if err != nil {
		return err
	}

	store, err := listsOf(command)
	if err != nil {
		return err
	}
	env := &citizen.Environment{Session: sess, Installs: installs, Tool: name, Lists: store}
	return iface.Run(command.Context(), env, in, words, iface.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
}

// helpCommand shows the help of a command of arc, or of an app.
func helpCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command, or an installed app",
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 0 {
				return root.Help()
			}
			if found, _, err := root.Find(args); err == nil && found != root {
				return found.Help()
			}
			return runCapability(command, args[0], append(args[1:], "help"))
		},
	}
}

// builtinName says whether a name is a command of arc itself.
func builtinName(root *cobra.Command, name string) bool {
	for _, c := range root.Commands() {
		if c.Name() == name || slices.Contains(c.Aliases, name) {
			return true
		}
	}
	return name == "help" || name == "completion"
}
