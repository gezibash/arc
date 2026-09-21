package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/relays"
	"github.com/spf13/cobra"
)

func joinCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "join <host[:port]>",
		Short: "Pin the key of a relay, and make it the default relay",
		Long: "Join asks the relay for its public key and shows it. It saves the key\n" +
			"only after you type yes. With --relay-pubkey, it checks the relay\n" +
			"against that key and asks nothing. A relay that later answers with\n" +
			"another key is refused. On a machine with no key, join makes one.",
		Args: cobra.ExactArgs(1),
		RunE: join,
	}
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

	// A selector that names no key fails the join before any relay is asked,
	// and join never picks another identity in its place.
	makeKey, err := needsFirstKey(keys)
	if err != nil {
		return err
	}

	store := &relays.Store{Dir: keys.Dir}
	document, err := store.Load()
	if err != nil {
		return err
	}

	// The pin comes from --relay-pubkey, then from an earlier join of this
	// address. Only a relay with no pin needs a question.
	pin, _ := command.Flags().GetString("relay-pubkey")
	if pin != "" {
		if pin, err = relays.NormalizePin(pin); err != nil {
			return err
		}
	}
	saved := document.Relays[address]
	if pin != "" && saved != "" && pin != saved {
		return fmt.Errorf("%s is pinned to %s, and --relay-pubkey names another key: joining never changes a pin", address, saved)
	}
	if pin == "" {
		pin = saved
	}

	ctx, cancel := deadline(30)
	defer cancel()

	if pin == "" {
		found, err := relayKey(ctx, address, nil)
		if err != nil {
			return err
		}
		if !trusted(address, found, os.Stdin) {
			return errors.New("the relay key is not trusted, and nothing was saved")
		}
		pin = hex.EncodeToString(found)
	}

	// The handshake proves that the relay holds the pinned key.
	raw, _ := hex.DecodeString(pin)
	if _, err := relayKey(ctx, address, raw); err != nil {
		return err
	}
	if err := store.Remember(address, pin); err != nil {
		return err
	}
	fmt.Printf("joined %s\n%s\n", address, pin)

	if makeKey {
		me, err := keys.Generate()
		if err != nil {
			return err
		}
		if err := keys.SetDefault(me.Name()); err != nil {
			return err
		}
		fmt.Printf("made the identity %s\n%s\n", me.Name(), me.EncodePublicKey())
	}

	warnOverrides(address, pin)
	return nil
}

// needsFirstKey says whether join makes the first key of this machine. That
// is so only when no selector is there and the store holds no key.
func needsFirstKey(keys *identity.Store) (bool, error) {
	_, _, err := keys.Active()
	if !errors.Is(err, identity.ErrNoDefault) {
		return false, err
	}

	held, err := keys.List()
	if err != nil {
		return false, err
	}
	if len(held) > 0 {
		return false, errors.New("the store holds keys, and none is selected: pick one with arc keys use NAME")
	}
	return true, nil
}

// relayKey joins the relay with a temporary identity, and returns the key
// that it presents. With a pin, the handshake refuses any other key. The
// temporary identity never replaces the route of a running citizen.
func relayKey(ctx context.Context, address string, pin []byte) ([]byte, error) {
	probe, err := identity.Generate()
	if err != nil {
		return nil, err
	}

	connection, err := client.Dial(ctx, address, client.Options{Identity: probe, RelayPublicKey: pin})
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	return connection.RelayPublicKey(), nil
}

// trusted shows the key of a relay, and asks for an explicit yes.
func trusted(address string, key []byte, input io.Reader) bool {
	fmt.Printf("relay  %s\nkey    %s  (%s)\n", address, hex.EncodeToString(key), identity.Name(key))
	fmt.Print("Compare the key with the one that the operator gave you. Type yes to trust it: ")

	answer, _ := bufio.NewReader(input).ReadString('\n')

	// A terminal echoes the answer and its newline. Other input does not.
	if file, ok := input.(*os.File); !ok || !isTerminal(file) {
		fmt.Println()
	}
	return strings.EqualFold(strings.TrimSpace(answer), "yes")
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// warnOverrides reports settings of the shell that hide the relay that join
// saved. Join does not change them.
func warnOverrides(address, pin string) {
	if given := os.Getenv("ARC_RELAY"); given != "" {
		if normal, err := relays.NormalizeAddress(given); err != nil || normal != address {
			fmt.Fprintf(os.Stderr, "ARC_RELAY is %s, so commands in this shell use it, not %s. Unset it to use the relay that you joined.\n", given, address)
		}
	}
	if given := os.Getenv("ARC_RELAY_PUBKEY"); given != "" {
		if normal, err := relays.NormalizePin(given); err != nil || normal != pin {
			fmt.Fprintln(os.Stderr, "ARC_RELAY_PUBKEY pins another key, so commands in this shell use it. Unset it to use the key that you joined.")
		}
	}
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
	// Status asks with a temporary identity. It reads no selector, needs no
	// key, and never replaces the route of a citizen.
	held, err := openAnonymous(command)
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
