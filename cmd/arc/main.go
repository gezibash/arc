// Command arc is the command line tool of ARC.
//
//	arc keys gen            make an identity
//	arc join <host:port>    remember a relay and its public key
//	arc serve <uri>         serve a provider over the relay
//	arc call <address>      call a capability of another citizen
//	arc discover <query>    search the directory of the relay
//	arc status              ask the relay about itself
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// version is the release of this build. A build sets it with
// -ldflags "-X main.version=0.6.0".
var version = "dev"

func main() {
	command := root()

	// A name that arc does not hold may be a capability that this citizen
	// installed.
	if name, ok := firstName(os.Args[1:]); ok && !holds(command, name) {
		ran, err := runInstalled(name, os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "arc:", err)
			os.Exit(1)
		}
		if ran {
			return
		}
	}

	if err := command.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "arc:", err)
		os.Exit(1)
	}
}

// firstName reads the first word of the line that names a command. The
// flags of arc itself may stand before it.
func firstName(args []string) (string, bool) {
	takesValue := map[string]bool{
		"--relay": true, "--relay-pubkey": true, "--key": true, "--store": true,
	}

	for index := 0; index < len(args); index++ {
		argument := args[index]

		if strings.HasPrefix(argument, "-") {
			if takesValue[argument] {
				index++
			}
			continue
		}
		return argument, true
	}
	return "", false
}

// holds says whether arc itself answers to a name.
func holds(command *cobra.Command, name string) bool {
	for _, child := range command.Commands() {
		if child.Name() == name || child.HasAlias(name) {
			return true
		}
	}
	return name == "help" || name == "completion"
}

func root() *cobra.Command {
	command := &cobra.Command{
		Use:          "arc",
		Short:        "ARC: capabilities between citizens, addressed by public key",
		Version:      version,
		SilenceUsage: true,
		// main prints each error once, after "arc:".
		SilenceErrors: true,
	}

	command.PersistentFlags().String("relay", "", "the relay to use, as host:port (ARC_RELAY)")
	command.PersistentFlags().String("relay-pubkey", "", "the public key to pin for the relay (ARC_RELAY_PUBKEY)")
	command.PersistentFlags().String("key", "", "the identity to use, by petname (ARC_KEY)")
	command.PersistentFlags().String("store", "", "the directory of ARC (default ~/.config/arc)")

	command.AddCommand(
		installCommand(),
		group(toolCommand()),
		group(trustCommand()),
		group(keysCommand()),
		whoamiCommand(),
		joinCommand(),
		statusCommand(),
		serveCommand(),
		callCommand(),
		discoverCommand(),
		resolveCommand(),
		infoCapabilityCommand(),
		sendCommand(),
		listenCommand(),
		publishCommand(),
		group(listsCommand()),
		updateCommand(),
		group(appsCommand()),
		group(cacheCommand()),
		versionCommand(),
	)
	return command
}

// group makes a command that only holds subcommands. Without a subcommand it
// shows its help. An unknown subcommand is an error, so that a script that
// calls a command that does not exist fails.
func group(command *cobra.Command) *cobra.Command {
	command.Args = cobra.NoArgs
	command.RunE = func(command *cobra.Command, _ []string) error {
		return command.Help()
	}
	return command
}
