package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/control"
	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/lists"
	"github.com/spf13/cobra"
)

// The commands that reach another citizen: read what it offers, send it a
// message, and wait for one.

func infoCapabilityCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "info <public key> [capability]",
		Short: "Show the signed capability of another citizen",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  showCapability,
	}

	command.Flags().Bool("json", false, "write the signed package as JSON")
	return command
}

func showCapability(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	peer, err := peerKey(args[0])
	if err != nil {
		return err
	}

	capabilityID := "primary"
	if len(args) == 2 {
		capabilityID = args[1]
	}

	ctx, cancel := deadline(30)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	signed, err := connection.Peers().Detail(ctx, peer, capabilityID)
	if err != nil {
		return err
	}

	if asJSON, _ := command.Flags().GetBool("json"); asJSON {
		return write(signed)
	}

	provider, _ := signed["provider"].(map[string]any)
	fields, _ := signed["capability"].(map[string]any)
	invocation, _ := fields["invocation"].(map[string]any)
	release, _ := signed["release"].(map[string]any)

	fmt.Printf("%s\n  %s\n", text(provider["name"]), text(provider["public_key"]))
	fmt.Printf("\n%s (%s)\n", text(fields["title"]), text(fields["id"]))
	fmt.Printf("  %s\n", text(fields["summary"]))
	fmt.Printf("  address  %s+arc://%s%s\n",
		text(fields["scheme"]), text(provider["public_key"]), text(invocation["path"]))
	fmt.Printf("  call     %s %s\n", text(invocation["method"]), text(invocation["path"]))
	fmt.Printf("  version  %s (%s)\n", text(release["version"]), text(release["channel"]))
	fmt.Printf("  hash     %s\n", text(signed["package_hash"]))
	return nil
}

func sendCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "send <citizen> [message]",
		Short: "Send one message to another citizen",
		Long: "The citizen is a public key, a petname, or the start of a key.\n" +
			"Without a message on the command line, the message comes from\n" +
			"the standard input.",
		Args: cobra.RangeArgs(1, 2),
		RunE: sendMessage,
	}

	command.Flags().Bool("wait", false, "wait for an answer")
	return command
}

func sendMessage(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	ctx, cancel := deadline(30)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	peer, err := findPeer(ctx, held, connection, args[0])
	if err != nil {
		return err
	}

	var body []byte
	if len(args) == 2 {
		body = []byte(args[1])
	} else {
		if body, err = io.ReadAll(io.LimitReader(os.Stdin, client.MaxBodyBytes+1)); err != nil {
			return err
		}
	}

	peers := connection.Peers()
	wait, _ := command.Flags().GetBool("wait")

	if !wait {
		// Without an answer to wait for, the message goes as an event.
		event, err := frame.EncodeEvent("arc.message.v1", body, nil)
		if err != nil {
			return err
		}
		if err := peers.SendFrame(peer, event); err != nil {
			return err
		}

		fmt.Printf("sent to %s\n", identity.Name(peer))
		return nil
	}

	answer, err := peers.Request(ctx, peer, map[string]any{"method": "RAW", "path": "/"}, body)
	if err != nil {
		return err
	}

	os.Stdout.Write(answer.Body)
	if len(answer.Body) > 0 && !strings.HasSuffix(string(answer.Body), "\n") {
		fmt.Println()
	}
	return nil
}

func listenCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "listen",
		Short: "Join the relay and print the messages that arrive",
		Args:  cobra.NoArgs,
		RunE:  listen,
	}

	command.Flags().Bool("json", false, "write each message as JSON")
	return command
}

func listen(command *cobra.Command, _ []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	connection, err := client.Dial(ctx, held.relay.Address, client.Options{
		Identity: held.me, RelayPublicKey: held.relay.Pin,
	})
	if err != nil {
		return err
	}
	defer connection.Close()

	fmt.Printf("%s listens on %s\n%s\n",
		held.me.Name(), held.relay.Address, held.me.EncodePublicKey())

	peers := connection.Peers()
	asJSON, _ := command.Flags().GetBool("json")

	for {
		select {
		case event, ok := <-peers.Events():
			if !ok {
				return connection.Err()
			}

			if asJSON {
				write(map[string]any{
					"from": hex.EncodeToString(event.From),
					"name": identity.Name(event.From),
					"type": event.Frame.Type.String(),
					"meta": event.Frame.Meta,
					"body": string(event.Frame.Body),
				})
				continue
			}

			fmt.Printf("\n%s  %s\n%s\n",
				time.Now().Format("15:04:05"), identity.Name(event.From), event.Frame.Body)

		case <-connection.Done():
			return connection.Err()
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "the citizen stops listening")
			return nil
		}
	}
}

func publishCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "publish",
		Short: "Publish this identity on this machine, so other commands find it",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			held, err := open(command, false)
			if err != nil {
				return err
			}

			plane, err := controlStore(command)
			if err != nil {
				return err
			}

			entry, err := plane.Publish(held.me)
			if err != nil {
				return err
			}

			fmt.Printf("%s\n%s\n", entry.Name, entry.PublicKey)
			return nil
		},
	}
}

func listsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "lists",
		Short: "Saved sets of peers, one for each command",
	}

	command.AddCommand(
		&cobra.Command{
			Use: "add <command> <name> <citizen>...", Short: "Put citizens in a list",
			Args: cobra.MinimumNArgs(3),
			RunE: func(held *cobra.Command, args []string) error {
				store, err := listStore(held)
				if err != nil {
					return err
				}

				members, err := store.Add(args[0], args[1], args[2:])
				if err != nil {
					return err
				}

				fmt.Printf("%s/%s: %s\n", args[0], args[1], strings.Join(members, " "))
				return nil
			},
		},
		&cobra.Command{
			Use: "rm <command> <name> [citizen...]", Short: "Take citizens out of a list, or remove the list",
			Args: cobra.MinimumNArgs(2),
			RunE: func(held *cobra.Command, args []string) error {
				store, err := listStore(held)
				if err != nil {
					return err
				}

				members, err := store.Remove(args[0], args[1], args[2:])
				if err != nil {
					return err
				}
				if len(args) == 2 {
					fmt.Printf("removed %s/%s\n", args[0], args[1])
					return nil
				}

				fmt.Printf("%s/%s: %s\n", args[0], args[1], strings.Join(members, " "))
				return nil
			},
		},
		&cobra.Command{
			Use: "ls <command> [name]", Short: "Show the lists of a command",
			Args: cobra.RangeArgs(1, 2),
			RunE: func(held *cobra.Command, args []string) error {
				store, err := listStore(held)
				if err != nil {
					return err
				}

				if len(args) == 2 {
					members := store.Members(args[0], args[1])
					if len(members) == 0 {
						fmt.Printf("no list %s/%s\n", args[0], args[1])
						return nil
					}

					fmt.Println(strings.Join(members, "\n"))
					return nil
				}

				names := store.Names(args[0])
				if len(names) == 0 {
					fmt.Printf("no lists for %s\n", args[0])
					return nil
				}

				for _, name := range names {
					fmt.Printf("%s: %s\n", name, strings.Join(store.Members(args[0], name), " "))
				}
				return nil
			},
		},
	)
	return command
}

// findPeer reads a citizen from a key, a petname on this machine, or a name
// that the relay answers.
func findPeer(ctx context.Context, held *settings, connection *client.Client, query string) ([]byte, error) {
	if key, err := peerKey(query); err == nil {
		return key, nil
	}

	plane := &control.Store{Dir: held.keys.Dir}
	if entries, err := plane.Resolve(query); err == nil && len(entries) == 1 {
		return entries[0].Key()
	}

	entries, err := connection.Resolve(ctx, query)
	if err != nil {
		return nil, err
	}

	switch len(entries) {
	case 0:
		return nil, fmt.Errorf("no citizen answers to %s", query)
	case 1:
		return hex.DecodeString(text(entries[0]["public_key"]))
	default:
		return nil, fmt.Errorf("%s names more than one citizen", query)
	}
}

func peerKey(value string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.ToLower(value))
	if err != nil || len(raw) != identity.SeedBytes {
		return nil, errors.New("a citizen is named by 64 characters of hex")
	}
	return raw, nil
}

func controlStore(command *cobra.Command) (*control.Store, error) {
	if dir, _ := command.Flags().GetString("store"); dir != "" {
		return &control.Store{Dir: dir}, nil
	}
	return control.DefaultStore()
}

func listStore(command *cobra.Command) (*lists.Store, error) {
	if dir, _ := command.Flags().GetString("store"); dir != "" {
		return &lists.Store{Dir: dir}, nil
	}
	return lists.DefaultStore()
}
