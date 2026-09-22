package main

import (
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/lists"
	"github.com/spf13/cobra"
)

// listsOf is the store of the lists of the chosen identity. A list belongs
// to one installed command, and installs belong to one identity.
func listsOf(command *cobra.Command) (*lists.Store, error) {
	dir, err := home(command)
	if err != nil {
		return nil, err
	}
	return &lists.Store{Dir: dir}, nil
}

// List returns the members of a list of the running command, as keys.
func (e *cliEnv) List(name string) []nostr.PubKey {
	if e.lists == nil {
		return nil
	}
	var out []nostr.PubKey
	for _, member := range e.lists.Members(e.tool, name) {
		if pk, err := nostr.PubKeyFromHex(member); err == nil {
			out = append(out, pk)
		}
	}
	return out
}

func listsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "lists",
		Short: "Saved sets of citizens, one for each installed command",
		Long: "A list names a set of citizens for one installed command. Where the\n" +
			"command takes a key, the name of a list runs it once for each member:\n" +
			"arc lists add dm team <key> <key>, then arc dm send team hello.",
	}
	command.AddCommand(
		&cobra.Command{
			Use: "add <command> <name> <citizen>...", Short: "Put citizens in a list",
			Args: cobra.MinimumNArgs(3),
			RunE: func(command *cobra.Command, args []string) error {
				installs, err := installsOf(command)
				if err != nil {
					return err
				}
				if _, ok := installs.Named(args[0]); !ok {
					return fmt.Errorf("no installed command %q: see arc tool list", args[0])
				}
				// A member is a key, in any form that a key argument takes.
				env := &cliEnv{installs: installs}
				var keys []string
				for _, text := range args[2:] {
					pk, err := env.ResolveKey(command.Context(), text)
					if err != nil {
						return err
					}
					keys = append(keys, pk.Hex())
				}
				store, err := listsOf(command)
				if err != nil {
					return err
				}
				held, err := store.Add(args[0], args[1], keys)
				if err != nil {
					return err
				}
				fmt.Printf("%s/%s: %s\n", args[0], args[1], strings.Join(held, " "))
				return nil
			},
		},
		&cobra.Command{
			Use: "rm <command> <name> [citizen...]", Short: "Take citizens out of a list, or remove the list",
			Args: cobra.MinimumNArgs(2),
			RunE: func(command *cobra.Command, args []string) error {
				installs, err := installsOf(command)
				if err != nil {
					return err
				}
				env := &cliEnv{installs: installs}
				var keys []string
				for _, text := range args[2:] {
					pk, err := env.ResolveKey(command.Context(), text)
					if err != nil {
						return err
					}
					keys = append(keys, pk.Hex())
				}
				store, err := listsOf(command)
				if err != nil {
					return err
				}
				held, err := store.Remove(args[0], args[1], keys)
				if err != nil {
					return err
				}
				if len(keys) == 0 {
					fmt.Printf("removed %s/%s\n", args[0], args[1])
					return nil
				}
				fmt.Printf("%s/%s: %s\n", args[0], args[1], strings.Join(held, " "))
				return nil
			},
		},
		&cobra.Command{
			Use: "ls <command> [name]", Short: "Show the lists of a command",
			Args: cobra.RangeArgs(1, 2),
			RunE: func(command *cobra.Command, args []string) error {
				store, err := listsOf(command)
				if err != nil {
					return err
				}
				if len(args) == 2 {
					held := store.Members(args[0], args[1])
					if len(held) == 0 {
						fmt.Printf("no list %s/%s\n", args[0], args[1])
						return nil
					}
					fmt.Println(strings.Join(held, "\n"))
					return nil
				}
				names := store.Names(args[0])
				if len(names) == 0 {
					fmt.Printf("no lists for %s\n", args[0])
					return nil
				}
				for _, name := range names {
					fmt.Printf("%s: %s\n", name, strings.Join(store.Members(args[0], name), " "))
				}
				return nil
			},
		},
	)
	return command
}
