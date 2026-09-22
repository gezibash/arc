package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/bundle"
	"github.com/gezibash/arc/citizen"
	"github.com/spf13/cobra"
)

func serveCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "serve <uri|directory>",
		Short: "Serve a provider over the relay",
		Long: "The address names the provider program and its manifest:\n\n" +
			"  exec:///path/to/runtime?manifest=/path/to/capability.json\n\n" +
			"A directory that holds an Arcfile works as well. Write one with\n" +
			"arc apps init.\n\n" +
			"The citizen announces the capability, and passes each request to\n" +
			"the program. It serves until you stop it.",
		Args: cobra.ExactArgs(1),
		RunE: serve,
	}

	command.Flags().String("direct-policy", "",
		"a file of rules that let a named peer carry a conversation off the relay")
	federationFlags(command)
	return command
}

// federationFlags adds the flags that choose how far an announcement
// travels. Without them, it stays on the relay.
func federationFlags(command *cobra.Command) {
	command.Flags().Bool("federate", false, "share the announcement with the partners of the relay")
	command.Flags().Bool("federate-network", false,
		"share the announcement across the relays that pass traffic on")
	command.MarkFlagsMutuallyExclusive("federate", "federate-network")
}

// reach reads the federation flags.
func reach(command *cobra.Command) announce.Federation {
	if network, _ := command.Flags().GetBool("federate-network"); network {
		return announce.Network
	}
	if direct, _ := command.Flags().GetBool("federate"); direct {
		return announce.Direct
	}
	return announce.Local
}

func serve(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	policy, _ := command.Flags().GetString("direct-policy")

	address, held2, err := bundle.Resolve(args[0])
	if err != nil {
		return err
	}
	if held2 != nil {
		fmt.Fprintf(os.Stderr, "the bundle %s runs %s\n", held2.Root, held2.Command)
	}

	serving, err := citizen.Serve(ctx, citizen.Options{
		Identity:       held.me,
		Relay:          held.relay.Address,
		RelayPublicKey: held.relay.Pin,
		Serve:          address,
		DirectPolicy:   policy,
		Federation:     reach(command),
		Log:            stderrLog(),
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s serves on %s\n%s\n", held.me.Name(), held.relay.Address, held.me.EncodePublicKey())

	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "the citizen is stopping")
		return serving.Close()
	case <-serving.Done():
	}

	// The citizen stopped by itself. Its reason comes first: the provider's
	// own exit status only follows from it.
	stopped := serving.Close()
	cause := serving.Err()
	switch {
	case cause == nil:
		return stopped
	case stopped != nil:
		return fmt.Errorf("%w (%v)", cause, stopped)
	default:
		return cause
	}
}
