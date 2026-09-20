package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gezibash/arc/go/client"
	"github.com/gezibash/arc/go/direct"
	"github.com/gezibash/arc/go/identity"
	"github.com/spf13/cobra"
)

func callCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "call <address> [body]",
		Short: "Call a capability of another citizen",
		Long: "The address names the capability and the resource:\n\n" +
			"  exec+arc://<public key>/<path>\n\n" +
			"Without a body on the command line, the body comes from standard\n" +
			"input. The signed package of the citizen names the method.",
		Args: cobra.RangeArgs(1, 2),
		RunE: call,
	}

	command.Flags().String("capability", "primary", "the capability to call")
	command.Flags().Int("timeout", 30, "how many seconds to wait for the answer")
	command.Flags().Bool("manifest", false, "show what the citizen offers, and call nothing")
	command.Flags().String("direct-policy", "",
		"a file of rules that let this conversation leave the relay")
	return command
}

func call(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	target, err := client.ParseAddress(args[0])
	if err != nil {
		return err
	}

	seconds, _ := command.Flags().GetInt("timeout")
	ctx, cancel := deadline(seconds)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	peers := connection.Peers()

	if manifest, _ := command.Flags().GetBool("manifest"); manifest {
		summary, err := peers.Manifest(ctx, target.Key)
		if err != nil {
			return err
		}
		return write(summary)
	}

	capabilityID, _ := command.Flags().GetString("capability")

	// With a policy of the owner, the conversation leaves the relay before
	// the call.
	if policy, _ := command.Flags().GetString("direct-policy"); policy != "" {
		rules, err := direct.LoadPolicy(policy)
		if err != nil {
			return err
		}

		peers.Direct(rules, stderrLog())
		if err := peers.Promote(ctx, args[0], capabilityID); err != nil {
			return fmt.Errorf("the conversation did not leave the relay: %w", err)
		}
	}

	body, err := requestBody(command, args)
	if err != nil {
		return err
	}
	answer, err := peers.Call(ctx, args[0], body, capabilityID)
	if err != nil {
		return err
	}

	os.Stdout.Write(answer.Body)
	if len(answer.Body) > 0 && !strings.HasSuffix(string(answer.Body), "\n") {
		fmt.Println()
	}
	return nil
}

// requestBody takes the body from the command line, or from standard input.
func requestBody(command *cobra.Command, args []string) ([]byte, error) {
	if len(args) == 2 {
		return []byte(args[1]), nil
	}

	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(os.Stdin, client.MaxBodyBytes+1))
}

func discoverCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "discover [query]",
		Short: "Search the directory of the relay",
		Args:  cobra.MaximumNArgs(1),
		RunE:  discover,
	}

	command.Flags().Int("limit", 10, "how many citizens to show")
	command.Flags().Bool("json", false, "write the answer as JSON")
	return command
}

func discover(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	query := ""
	if len(args) == 1 {
		query = args[0]
	}

	ctx, cancel := deadline(30)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	limit, _ := command.Flags().GetInt("limit")
	page, err := connection.Search(ctx, query, limit, "")
	if err != nil {
		return err
	}

	if asJSON, _ := command.Flags().GetBool("json"); asJSON {
		return write(page.Entries)
	}

	if len(page.Entries) == 0 {
		fmt.Println("the relay holds no citizen that offers this")
		return nil
	}

	for _, entry := range page.Entries {
		show(entry)
	}
	if page.Next != "" {
		fmt.Printf("\n%d citizens in all\n", page.Total)
	}
	return nil
}

func resolveCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "resolve <name or key>",
		Short: "Find one citizen by petname, or by the start of its key",
		Args:  cobra.ExactArgs(1),
		RunE:  resolve,
	}

	command.Flags().Bool("json", false, "write the answer as JSON")
	return command
}

func resolve(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	// A citizen of this machine answers without a relay.
	if plane, err := controlStore(command); err == nil {
		if entries, err := plane.Resolve(args[0]); err == nil && len(entries) > 0 {
			asJSON, _ := command.Flags().GetBool("json")
			if asJSON {
				return write(entries)
			}

			for _, entry := range entries {
				fmt.Printf("%s\n  %s\n  on this machine\n", entry.Name, entry.PublicKey)
			}
			return nil
		}
	}

	ctx, cancel := deadline(30)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	entries, err := connection.Resolve(ctx, args[0])
	if err != nil {
		return err
	}

	if asJSON, _ := command.Flags().GetBool("json"); asJSON {
		return write(entries)
	}
	if len(entries) == 0 {
		fmt.Printf("no citizen answers to %s\n", args[0])
		return nil
	}

	for _, entry := range entries {
		show(entry)
	}
	return nil
}

// show writes one announcement for a person to read.
func show(entry map[string]any) {
	key, _ := entry["public_key"].(string)
	raw, err := hex.DecodeString(key)
	if err != nil {
		return
	}

	fmt.Printf("%s\n  %s\n", identity.Name(raw), key)

	capabilities, _ := entry["capabilities"].([]any)
	for _, item := range capabilities {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}

		id, _ := fields["id"].(string)
		scheme, _ := fields["scheme"].(string)
		title, _ := fields["title"].(string)
		fmt.Printf("  %s+arc://%s/  %s (%s)\n", orDash(scheme), key, orDash(title), id)
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
