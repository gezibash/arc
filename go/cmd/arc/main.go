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

	"github.com/spf13/cobra"
)

// version is the release of this build. A build sets it with
// -ldflags "-X main.version=0.6.0".
var version = "dev"

func main() {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "arc:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	command := &cobra.Command{
		Use:          "arc",
		Short:        "ARC: capabilities between citizens, addressed by public key",
		Version:      version,
		SilenceUsage: true,
	}

	command.PersistentFlags().String("relay", "", "the relay to use, as host:port (ARC_RELAY)")
	command.PersistentFlags().String("relay-pubkey", "", "the public key to pin for the relay (ARC_RELAY_PUBKEY)")
	command.PersistentFlags().String("key", "", "the identity to use, by petname (ARC_KEY)")
	command.PersistentFlags().String("store", "", "the directory of ARC (default ~/.config/arc)")

	command.AddCommand(
		keysCommand(),
		whoamiCommand(),
		joinCommand(),
		statusCommand(),
		serveCommand(),
		callCommand(),
		discoverCommand(),
		resolveCommand(),
	)
	return command
}
