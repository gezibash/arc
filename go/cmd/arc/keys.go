package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func keysCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "keys",
		Short: "Make, list and pick identities",
	}

	command.AddCommand(
		&cobra.Command{
			Use:   "gen",
			Short: "Make an identity, and make it the default when there is none",
			Args:  cobra.NoArgs,
			RunE:  generateKey,
		},
		&cobra.Command{
			Use:   "list",
			Short: "List the identities of this machine",
			Args:  cobra.NoArgs,
			RunE:  listKeys,
		},
		&cobra.Command{
			Use:   "use <name>",
			Short: "Make one identity the default",
			Args:  cobra.ExactArgs(1),
			RunE:  useKey,
		},
		&cobra.Command{
			Use:   "remove <name>",
			Short: "Remove one identity from this machine",
			Args:  cobra.ExactArgs(1),
			RunE:  removeKey,
		},
	)
	return command
}

func generateKey(command *cobra.Command, _ []string) error {
	dir, _ := command.Flags().GetString("store")
	keys, err := keyStore(dir)
	if err != nil {
		return err
	}

	me, err := keys.Generate()
	if err != nil {
		return err
	}

	if _, err := keys.Default(); err != nil {
		if err := keys.SetDefault(me.Name()); err != nil {
			return err
		}
	}

	fmt.Printf("%s\n%s\n", me.Name(), me.EncodePublicKey())
	return nil
}

func listKeys(command *cobra.Command, _ []string) error {
	dir, _ := command.Flags().GetString("store")
	keys, err := keyStore(dir)
	if err != nil {
		return err
	}

	identities, err := keys.List()
	if err != nil {
		return err
	}
	if len(identities) == 0 {
		fmt.Println("no identities: make one with arc keys gen")
		return nil
	}

	active, _ := keys.Default()
	for _, me := range identities {
		mark := " "
		if active != nil && string(active.PublicKey) == string(me.PublicKey) {
			mark = "*"
		}
		fmt.Printf("%s %s  %s\n", mark, me.Name(), me.EncodePublicKey())
	}
	return nil
}

func useKey(command *cobra.Command, args []string) error {
	dir, _ := command.Flags().GetString("store")
	keys, err := keyStore(dir)
	if err != nil {
		return err
	}

	if err := keys.SetDefault(args[0]); err != nil {
		return err
	}

	me, err := keys.Default()
	if err != nil {
		return err
	}
	fmt.Printf("the default is %s\n", me.Name())
	return nil
}

func removeKey(command *cobra.Command, args []string) error {
	dir, _ := command.Flags().GetString("store")
	keys, err := keyStore(dir)
	if err != nil {
		return err
	}

	me, err := keys.Get(args[0])
	if err != nil {
		return err
	}
	if err := keys.Remove(args[0]); err != nil {
		return err
	}

	fmt.Printf("removed %s\n", me.Name())
	return nil
}

func whoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the identity that commands use here",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			held, err := open(command, false)
			if err != nil {
				return err
			}

			source := "the default key"
			if _, from, err := held.keys.Active(); err == nil {
				source = string(from)
			}

			fmt.Printf("%s\n%s\nchosen by %s\n", held.me.Name(), held.me.EncodePublicKey(), source)
			return nil
		},
	}
}
