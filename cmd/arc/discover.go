package main

import (
	"fmt"
	"os"

	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func discoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover [query]",
		Short: "Find apps announced by participants",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			_, errs := sess.Node.Pull(command.Context(), announcements, sess.Relays)
			for _, err := range errs {
				fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%v", err)))
			}

			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			offers, err := catalog.Search(sess.Node.Store, query)
			if err != nil {
				return err
			}
			if len(offers) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no apps found: add a relay, or sync with a directory")
			}
			for _, o := range offers {
				fmt.Fprintf(command.OutOrStdout(), "%s/%s  %s\n  %s\n  %s\n", o.Name(), terminal.EscapeLine(o.ID), terminal.EscapeLine(o.Title), shown(o.Summary), o.Provider.Hex()) //arc:raw-ok a petname that arc makes from the key
			}
			return nil
		},
	}
}
