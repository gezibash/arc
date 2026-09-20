package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gezibash/arc/go/citizen"
	"github.com/spf13/cobra"
)

func serveCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "serve <uri>",
		Short: "Serve a provider over the relay",
		Long: "The address names the provider program and its manifest:\n\n" +
			"  exec:///path/to/runtime?manifest=/path/to/capability.json\n\n" +
			"The citizen announces the capability, and passes each request to\n" +
			"the program. It serves until you stop it.",
		Args: cobra.ExactArgs(1),
		RunE: serve,
	}

	command.Flags().String("direct-policy", "",
		"a file of rules that let a named peer carry a conversation off the relay")
	return command
}

func serve(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	policy, _ := command.Flags().GetString("direct-policy")

	serving, err := citizen.Serve(ctx, citizen.Options{
		Identity:       held.me,
		Relay:          held.relay.Address,
		RelayPublicKey: held.relay.Pin,
		Serve:          args[0],
		DirectPolicy:   policy,
		Log:            stderrLog(),
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s serves on %s\n%s\n", held.me.Name(), held.relay.Address, held.me.EncodePublicKey())

	select {
	case <-ctx.Done():
	case <-serving.Done():
	}

	fmt.Fprintln(os.Stderr, "the citizen is stopping")
	return serving.Close()
}
