package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/gezibash/arc/go/relays"
	"github.com/spf13/cobra"
)

func joinCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "join <host:port>",
		Short: "Remember a relay and pin its public key",
		Long: "Join asks the relay for its public key, shows it, and saves it.\n" +
			"A relay that later answers with another key is refused.",
		Args: cobra.ExactArgs(1),
		RunE: join,
	}

	command.Flags().String("pubkey", "", "the public key to pin, when it is known already")
	return command
}

func join(command *cobra.Command, args []string) error {
	dir, _ := command.Flags().GetString("store")
	keys, err := keyStore(dir)
	if err != nil {
		return err
	}

	address, err := relays.NormalizeAddress(args[0])
	if err != nil {
		return err
	}

	pin, _ := command.Flags().GetString("pubkey")
	me, err := activeIdentity(keys, "")
	if err != nil {
		return err
	}

	ctx, cancel := deadline(15)
	defer cancel()

	// The handshake proves the key of the relay. A pin that the user gave
	// must match it.
	held := &settings{keys: keys, relays: &relays.Store{Dir: keys.Dir}, me: me}
	held.relay = &relays.Selection{Address: address}
	if pin != "" {
		key, err := relays.NormalizePin(pin)
		if err != nil {
			return err
		}
		if held.relay.Pin, err = hex.DecodeString(key); err != nil {
			return err
		}
	}

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	found := hex.EncodeToString(connection.RelayPublicKey())
	if err := held.relays.Remember(address, found); err != nil {
		return err
	}

	fmt.Printf("joined %s\n%s\n", address, found)
	return nil
}

func statusCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "status",
		Short: "Ask the relay about itself",
		Args:  cobra.NoArgs,
		RunE:  status,
	}

	command.Flags().Bool("json", false, "write the answer as JSON")
	return command
}

func status(command *cobra.Command, _ []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	ctx, cancel := deadline(15)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	answer, err := connection.Status(ctx)
	if err != nil {
		return err
	}

	if asJSON, _ := command.Flags().GetBool("json"); asJSON {
		return write(answer)
	}

	fmt.Printf("relay    %s\n", held.relay.Address)
	fmt.Printf("state    %v\n", answer["state"])
	fmt.Printf("version  %v\n", answer["version"])
	fmt.Printf("uptime   %v seconds\n", answer["uptime_seconds"])
	fmt.Printf("key      %v\n", answer["public_key"])
	return nil
}

// write prints one value as JSON.
func write(value any) error {
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
