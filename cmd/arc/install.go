package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func installCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "install <author-or-service> [app]",
		Short: "Install an app's commands and record your consent",
		Long: "Install the signed interface and permissions; no executable is downloaded.\n" +
			"A data app runs locally. A service app calls the selected participant.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			if as, _ := command.Flags().GetString("as"); controlOrFormat(as) {
				return errors.New("--as: a name has no control character and no format character")
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			installs, err := installsOf(command)
			if err != nil {
				return err
			}
			provider, id, err := installs.Resolve(command.Context(), args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				id = args[1]
			}

			offer, err := sess.FindOffer(command.Context(), provider, id)
			if err != nil {
				return err
			}

			showOffer(command.OutOrStdout(), offer)
			if yes, _ := command.Flags().GetBool("yes"); !yes {
				fmt.Fprint(command.OutOrStdout(), "Trust this app and its signing identity? [y/N] ")
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(answer)) != "y" {
					return errors.New("not installed")
				}
			}
			if offer.Manifest == nil {
				if err := installs.Add(offer, ""); err != nil {
					return err
				}
				fmt.Fprintf(command.OutOrStdout(), "installed %s: call it with arc call %s\n", terminal.EscapeLine(offer.ID), offer.Name()) //arc:raw-ok a petname
				return nil
			}
			as, _ := command.Flags().GetString("as")
			if as == "" {
				as = offer.ID
			}
			if builtinName(command.Root(), as) {
				return fmt.Errorf("%s is a name of arc itself; choose another name with --as", as)
			}
			if err := installs.Add(offer, as); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "installed %s: see arc help %s\n", terminal.EscapeLine(as), terminal.EscapeLine(as))
			return nil
		},
	}
	command.Flags().Bool("yes", false, "accept the app's permissions without asking")
	command.Flags().String("as", "", "the name that runs the app (default: its id)")
	return command
}
