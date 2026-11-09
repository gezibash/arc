// Command arc is the command line tool of ARC, on Nostr events. See
// docs/delivery/SPEC.md.
//
//	arc keys gen | recover | add | list | use | remove | encrypt | bunker
//	arc whoami
//	arc relay add <url> | rm <url> | ls | turn | serve
//	arc message send | inbox | outbox | watch
//	arc serve | announce | discover | install | call | session
//	arc sync [--dir <path>]
//	arc apps list | info | remove
//	arc info | resolve | apps init | version
//	arc lists add | rm | ls
//	arc update [check | apply]
//	arc release sign
//	arc <app> <command...>
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func main() {
	if err := root().Execute(); err != nil {
		// An app set the exit status from its reply, and wrote its
		// output already.
		if status, ok := errors.AsType[iface.ExitError](err); ok {
			os.Exit(status.Code)
		}
		fmt.Fprintln(os.Stderr, errorLine(err))
		os.Exit(1)
	}
}

// errorLine is the line that arc prints for an error. An error can carry
// text that another party chose, such as the refusal of a provider or of a
// relay, so the line is escaped and stays one line, as docs/clients/SPEC.md,
// section 3.9, says: a refusal cannot forge a second line.
func errorLine(err error) string {
	return "arc: " + terminal.EscapeLine(citizen.SignerOnly(err).Error())
}

func root() *cobra.Command {
	command := &cobra.Command{
		Use:   "arc",
		Short: "ARC on signed events, over any transport",
		Long: "ARC on signed events, over any transport.\n\n" +
			"An installed app adds its own commands: arc <name> <command>.\n" +
			"arc help <name> lists them.",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE:               dispatch,
		SilenceUsage:       true,
		SilenceErrors:      true,
	}
	command.PersistentFlags().String("home", "", "the directory of arc; arc takes --home, then ARC_HOME, then ~/.arc, then ~/.config/arc when an older arc made it; a ~/.arc with no identity gives way to a ~/.config/arc with one")
	command.PersistentFlags().String("key", "", "the identity to use, by petname (ARC_KEY)")
	command.AddCommand(keysCommand(), whoamiCommand(), relayCommand(), messageCommand(), contactsCommand(),
		serveCmd(), announceCmd(), discoverCmd(), installCmd(), callCmd(), sessionCmd(), syncCommand(),
		appsCommand(), infoCommand(), resolveCommand(), versionCommand(), listsCommand(), updateCommand(), releaseCommand(), pathCommand())
	command.SetHelpCommand(helpCommand(command))
	command.Version = version
	return command
}

// The three errors of a home that arc cannot read. Each case has one
// wording, which names the folder and the rule that chose it, and says
// what fixes it (#408).
