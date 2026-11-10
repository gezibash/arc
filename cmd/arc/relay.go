package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func relayCommand() *cobra.Command {
	command := &cobra.Command{Use: "relay", Short: "Manage the relays of this citizen"}

	command.AddCommand(
		relayAddCommand(),
		&cobra.Command{
			Use: "rm <url>", Short: "Stop using a relay", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				relays, err := readURLs(relaysPath(dir))
				if err != nil {
					return err
				}
				indexers, err := readURLs(indexersPath(dir))
				if err != nil {
					return err
				}
				other := func(u string) bool { return u == args[0] }
				if err := writeRelays(dir, slices.DeleteFunc(relays, other)); err != nil {
					return err
				}
				if err := writeIndexers(dir, slices.DeleteFunc(indexers, other)); err != nil {
					return err
				}
				announceRelays(command)
				return nil
			},
		},
		&cobra.Command{
			Use: "ls", Short: "List the relays", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				urls, err := readURLs(relaysPath(dir))
				if err != nil {
					return err
				}
				indexers, err := readURLs(indexersPath(dir))
				if err != nil {
					return err
				}
				if len(urls) == 0 {
					fmt.Fprintln(command.OutOrStdout(), "no relays: add one with arc relay add <url>")
				}
				// A list of an older arc can hold any character, and a
				// line that is not a relay URL, which arc skips.
				skipped := false
				show := func(url, suffix string) {
					if !isRelayURL(url) {
						fmt.Fprintf(command.OutOrStdout(), "not a relay URL, skipped: \"%s\"%s\n", terminal.EscapeLine(url), suffix)
						skipped = true
						return
					}
					fmt.Fprintln(command.OutOrStdout(), terminal.EscapeLine(url)+suffix)
				}
				for _, url := range urls {
					show(url, "")
				}
				for _, url := range indexers {
					show(url, "  (index)")
				}
				if skipped {
					fmt.Fprintln(command.ErrOrStderr(), "arc: the relay list holds a line that is not a relay URL, which arc skips; remove it with arc relay rm and the line as the list holds it, quoted for the shell")
				}
				return nil
			},
		},
		turnCommand(),
		ringCommand(),
	)
	return command
}

func relayAddCommand() *cobra.Command {
	add := &cobra.Command{
		Use: "add <url>", Short: "Send to and sync with a relay", Args: cobra.ExactArgs(1),
		Long: "With --index, the relay is an indexer: arc publishes only its relay\n" +
			"lists there, and looks up the relay lists of other citizens there.",
		RunE: func(command *cobra.Command, args []string) error {
			url := args[0]
			if err := checkRelayURL(url); err != nil {
				return err
			}
			dir, err := home(command)
			if err != nil {
				return err
			}
			// add, rm and ls read each line, so that a write keeps the
			// lines that arc skips.
			relays, err := readURLs(relaysPath(dir))
			if err != nil {
				return err
			}
			indexers, err := readURLs(indexersPath(dir))
			if err != nil {
				return err
			}
			index, _ := command.Flags().GetBool("index")
			if index {
				if slices.Contains(relays, url) {
					return fmt.Errorf("%s is a relay of this citizen; remove it with arc relay rm first", url)
				}
				if !slices.Contains(indexers, url) {
					indexers = append(indexers, url)
				}
				if err := writeIndexers(dir, indexers); err != nil {
					return err
				}
			} else {
				if slices.Contains(indexers, url) {
					return fmt.Errorf("%s is an indexer of this citizen; remove it with arc relay rm first", url)
				}
				if !slices.Contains(relays, url) {
					relays = append(relays, url)
				}
				if err := writeRelays(dir, relays); err != nil {
					return err
				}
			}
			announceRelays(command)
			return nil
		},
	}
	add.Flags().Bool("index", false, "use the relay only to publish and look up relay lists")
	return add
}

// announceRelays publishes the relay list of this citizen, when it has a key
// and at least one relay.
func announceRelays(command *cobra.Command) {
	sess, err := open(command)
	if err != nil {
		return
	}
	defer sess.Close()
	if len(sess.Relays) > 0 {
		publishRelayList(command.Context(), sess, os.Stderr, nil)
	}
}
