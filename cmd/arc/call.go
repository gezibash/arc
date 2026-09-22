package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/direct"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/wake"
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

		// A rule that does not match, or a peer that does not agree, leaves
		// the conversation on the relay.
		peers.Direct(rules, stderrLog())
		if err := peers.Promote(ctx, args[0], capabilityID); err != nil {
			fmt.Fprintf(os.Stderr, "the conversation stays on the relay: %v\n", err)
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

	// The body goes out as it came, so bytes stay bytes. Only a terminal
	// gets a newline after a body that has none.
	os.Stdout.Write(answer.Body)
	if len(answer.Body) > 0 && !strings.HasSuffix(string(answer.Body), "\n") && stdoutIsTerminal() {
		fmt.Println()
	}
	return nil
}

func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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
		show(entry, "")
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

	// A citizen that pauses has no announcement while it sleeps. The wake
	// hooks of this machine still name it.
	waker := held.waker()
	asleep := sleepers(waker, args[0], entries)

	if asJSON, _ := command.Flags().GetBool("json"); asJSON {
		for _, entry := range entries {
			entry["state"] = wake.Online
		}
		for _, key := range asleep {
			entries = append(entries, map[string]any{
				"public_key": hex.EncodeToString(key),
				"name":       identity.Name(key),
				"state":      wake.Asleep,
			})
		}
		return write(entries)
	}

	if len(entries) == 0 && len(asleep) == 0 {
		if key, err := hex.DecodeString(args[0]); err == nil && len(key) == identity.SeedBytes {
			fmt.Printf("%s\n  %s\n  %s\n", identity.Name(key), hex.EncodeToString(key), wake.Offline)
			return nil
		}
		fmt.Printf("no citizen answers to %s\n", args[0])
		return nil
	}

	for _, entry := range entries {
		show(entry, wake.Online)
	}
	for _, key := range asleep {
		fmt.Printf("%s\n  %s\n  %s: arc wakes it with its wake hook\n", identity.Name(key), hex.EncodeToString(key), waker.State(key, false))
	}
	return nil
}

// sleepers gives the citizens with a wake hook that the query names, and
// that the relay does not announce. The query matches the way the relay
// matches: the petname, the short name, or the start of the key.
func sleepers(waker *wake.Waker, query string, entries []map[string]any) [][]byte {
	query = strings.ToLower(query)
	if query == "" {
		return nil
	}

	announced := map[string]bool{}
	for _, entry := range entries {
		if key, ok := entry["public_key"].(string); ok {
			announced[strings.ToLower(key)] = true
		}
	}

	var found [][]byte
	for _, key := range waker.Citizens() {
		text := hex.EncodeToString(key)
		named := query == strings.ToLower(identity.Name(key)) || query == strings.ToLower(identity.ShortName(key))
		if (named || strings.HasPrefix(text, query)) && !announced[text] {
			found = append(found, key)
		}
	}
	return found
}

// show writes one announcement for a person to read. A state, when given,
// follows the key.
func show(entry map[string]any, state string) {
	key, _ := entry["public_key"].(string)
	raw, err := hex.DecodeString(key)
	if err != nil {
		return
	}

	fmt.Printf("%s\n  %s\n", identity.Name(raw), key)
	if state != "" {
		fmt.Printf("  %s\n", state)
	}

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
