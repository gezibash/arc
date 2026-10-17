package main

import (
	"fmt"
	"slices"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/spf13/cobra"
)

// contactsCommand keeps the contact list: the keys whose mail reaches the
// inbox without proof of work. docs/strangers/SPEC.md defines it.
func contactsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "contacts",
		Short: "List the keys whose mail reaches your inbox without proof of work",
		Long: "A message from any other key, a stranger, reaches the inbox only with\n" +
			"proof of work. Without it, arc holds the message apart: see arc\n" +
			"message held. arc adds each key that you send a message to. The list\n" +
			"is self data: arc sync gives it to your storage citizens, and never\n" +
			"to a relay.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			listed, err := sess.Contacts().List(command.Context())
			if err != nil {
				return err
			}
			if len(listed) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no contacts: arc adds each key that you send a message to")
			}
			slices.SortFunc(listed, func(a, b nostr.PubKey) int { return strings.Compare(a.Hex(), b.Hex()) })
			for _, pk := range listed {
				fmt.Fprintf(command.OutOrStdout(), "%s  %s\n", pk.Hex(), keys.Name(pk[:])) //arc:raw-ok a petname that arc makes from the key
			}
			return nil
		},
	}

	add := &cobra.Command{
		Use: "add <public key>[,<public key>...]", Short: "Add keys, and move their held messages to the inbox",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			pks, err := contactKeys(args[0])
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			if err := sess.Contacts().AddContacts(command.Context(), pks); err != nil {
				return err
			}
			report, err := sess.Mail.Release(command.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "added %s; moved %d held messages to the inbox\n", petnames(pks), report.Received)
			return nil
		},
	}

	rm := &cobra.Command{
		Use: "rm <public key>[,<public key>...]", Short: "Remove keys; their messages in the inbox stay",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			pks, err := contactKeys(args[0])
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			if err := sess.Contacts().Remove(command.Context(), pks); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "removed %s\n", petnames(pks))
			return nil
		},
	}

	command.AddCommand(add, rm)
	return command
}

// contactKeys reads public keys, separated by commas.
func contactKeys(text string) ([]nostr.PubKey, error) {
	var out []nostr.PubKey
	for part := range strings.SplitSeq(text, ",") {
		pk, err := nostr.PubKeyFromHex(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("a contact is 64 characters of hex, as arc whoami prints it; separate keys with commas")
		}
		out = append(out, pk)
	}
	return out, nil
}

// printHolding says on standard error how many messages from strangers
// wait, so that the filter is never silent.
func printHolding(command *cobra.Command, sess *citizen.Session) error {
	n, err := sess.Mail.Holding()
	if err != nil || n == 0 {
		return err
	}
	if n == 1 {
		fmt.Fprintln(command.ErrOrStderr(), "1 message from a stranger waits: arc message held")
		return nil
	}
	fmt.Fprintf(command.ErrOrStderr(), "%d messages from strangers wait: arc message held\n", n)
	return nil
}
