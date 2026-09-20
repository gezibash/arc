package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gezibash/arc/go/client"
	"github.com/gezibash/arc/go/control"
	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/toolbox"
	"github.com/spf13/cobra"
)

// An installed capability becomes a command of arc. The install holds the
// signed package that the owner accepted, so a later call checks that the
// citizen still serves the same thing.

func installCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "install <public key> [capability]",
		Short: "Install a capability of another citizen as a command",
		Long: "The citizen serves a signed package. arc checks the signature,\n" +
			"asks you about the signer once, and saves the package as a\n" +
			"command that you can run.",
		Args: cobra.RangeArgs(1, 2),
		RunE: install,
	}

	command.Flags().String("as", "", "the name of the command, when the capability suggests another")
	command.Flags().Bool("yes", false, "trust the signer without asking")
	return command
}

func install(command *cobra.Command, args []string) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	capabilityID := "primary"
	if len(args) == 2 {
		capabilityID = args[1]
	}

	peer, err := hex.DecodeString(strings.ToLower(args[0]))
	if err != nil || len(peer) != identity.SeedBytes {
		return errors.New("the citizen is named by 64 characters of hex")
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

	tools, err := toolStore(command)
	if err != nil {
		return err
	}

	signature, _ := signed["signature"].(map[string]any)
	signer, _ := signature["signer_public_key"].(string)

	state, err := tools.TrustState(held.me.PublicKey, signer)
	if err != nil {
		return err
	}

	switch state {
	case toolbox.Denied:
		return fmt.Errorf("you denied the signer %s: allow it with arc trust allow %s", short(signer), short(signer))
	case toolbox.Allowed:
	default:
		yes, _ := command.Flags().GetBool("yes")
		if !yes {
			if err := askAboutSigner(tools, held.me.PublicKey, signed, signer); err != nil {
				return err
			}
		} else if _, err := tools.Trust(held.me.PublicKey, signer, toolbox.Allowed, ""); err != nil {
			return err
		}
	}

	name, _ := command.Flags().GetString("as")
	record, err := tools.Install(held.me.PublicKey, signed, toolbox.Options{
		Command: name, TrustState: toolbox.Allowed,
	})
	if err != nil {
		return err
	}

	showInstall(record)
	return nil
}

// askAboutSigner asks the owner once about a signer it has not seen.
func askAboutSigner(tools *toolbox.Store, owner []byte, signed map[string]any, signer string) error {
	provider, _ := signed["provider"].(map[string]any)
	fields, _ := signed["capability"].(map[string]any)

	fmt.Printf("%s offers %s (%s)\n", text(provider["name"]), text(fields["title"]), text(fields["id"]))
	fmt.Printf("signer %s\n", signer)
	fmt.Printf("hash   %s\n", text(signed["package_hash"]))
	fmt.Print("Trust this signer? [y/N] ")

	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		_, err := tools.Trust(owner, signer, toolbox.Allowed, "")
		return err
	case "n", "no":
		if _, err := tools.Trust(owner, signer, toolbox.Denied, ""); err != nil {
			return err
		}
		return errors.New("the signer is denied, and nothing was installed")
	default:
		return errors.New("nothing was installed")
	}
}

func toolCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "tool",
		Short: "List, show and remove the capabilities that you installed",
	}

	command.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List the commands that you installed",
			Args: cobra.NoArgs, RunE: listTools,
		},
		infoCommand(),
		&cobra.Command{
			Use: "remove <command>", Short: "Remove one installed command",
			Args: cobra.ExactArgs(1), RunE: removeTool,
		},
		&cobra.Command{
			Use: "pin <command>", Short: "Hold one command at the version you installed",
			Args: cobra.ExactArgs(1), RunE: pinTool(true),
		},
		&cobra.Command{
			Use: "unpin <command>", Short: "Let one command move to a new version",
			Args: cobra.ExactArgs(1), RunE: pinTool(false),
		},
	)
	return command
}

func infoCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "info <command>", Short: "Show one installed command",
		Args: cobra.ExactArgs(1), RunE: toolInfo,
	}

	command.Flags().Bool("json", false, "write the install as JSON")
	return command
}

func listTools(command *cobra.Command, _ []string) error {
	held, tools, err := ownerTools(command)
	if err != nil {
		return err
	}

	installs, err := tools.List(held.me.PublicKey)
	if err != nil {
		return err
	}
	if len(installs) == 0 {
		fmt.Println("no commands: install one with arc install <public key>")
		return nil
	}

	fmt.Printf("%s holds %d commands\n", held.me.Name(), len(installs))
	for _, record := range installs {
		fields, _ := record["capability"].(map[string]any)
		provider, _ := record["provider"].(map[string]any)

		fmt.Printf("\n%s -> %s/%s\n", text(record["command"]), text(provider["name"]), text(record["capability_id"]))
		fmt.Printf("  usage   %s\n", text(record["usage"]))

		if subcommands := toolbox.Subcommands(fields); len(subcommands) > 0 {
			var labels []string
			for _, one := range subcommands {
				labels = append(labels, toolbox.Label(one))
			}
			fmt.Printf("  commands %s\n", strings.Join(labels, ", "))
		}
		fmt.Printf("  version %s (%s)\n", text(record["release_version"]), text(record["channel"]))
	}
	return nil
}

func toolInfo(command *cobra.Command, args []string) error {
	held, tools, err := ownerTools(command)
	if err != nil {
		return err
	}

	record, err := tools.Get(held.me.PublicKey, args[0])
	if err != nil {
		return err
	}

	if asJSON, _ := command.Flags().GetBool("json"); asJSON {
		return write(record)
	}

	showInstall(record)

	fields, _ := record["capability"].(map[string]any)
	for _, one := range toolbox.Commands(fields) {
		fmt.Printf("  %s\n", toolbox.CommandUsage(text(record["command"]), one))
		if summary := text(one["summary"]); summary != "" {
			fmt.Printf("      %s\n", summary)
		}
	}
	return nil
}

func removeTool(command *cobra.Command, args []string) error {
	held, tools, err := ownerTools(command)
	if err != nil {
		return err
	}

	if _, err := tools.Get(held.me.PublicKey, args[0]); err != nil {
		return err
	}
	if err := tools.Uninstall(held.me.PublicKey, args[0]); err != nil {
		return err
	}

	fmt.Printf("removed %s\n", toolbox.NormalizeCommand(args[0]))
	return nil
}

func pinTool(pinned bool) func(*cobra.Command, []string) error {
	return func(command *cobra.Command, args []string) error {
		held, tools, err := ownerTools(command)
		if err != nil {
			return err
		}

		record, err := tools.SetPinned(held.me.PublicKey, args[0], pinned)
		if err != nil {
			return err
		}

		state := "unpinned"
		if pinned {
			state = "pinned"
		}
		fmt.Printf("%s is %s at %s\n", text(record["command"]), state, text(record["release_version"]))
		return nil
	}
}

func trustCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "trust",
		Short: "Show and change what you decided about the signers of capabilities",
	}

	command.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List your decisions",
			Args: cobra.NoArgs, RunE: listTrust,
		},
		&cobra.Command{
			Use: "allow <signer>", Short: "Trust one signer",
			Args: cobra.ExactArgs(1), RunE: setTrust(toolbox.Allowed),
		},
		&cobra.Command{
			Use: "deny <signer>", Short: "Refuse one signer",
			Args: cobra.ExactArgs(1), RunE: setTrust(toolbox.Denied),
		},
	)
	return command
}

func listTrust(command *cobra.Command, _ []string) error {
	held, tools, err := ownerTools(command)
	if err != nil {
		return err
	}

	signers, err := tools.Signers(held.me.PublicKey)
	if err != nil {
		return err
	}
	if len(signers) == 0 {
		fmt.Println("no decisions yet")
		return nil
	}

	for _, signer := range signers {
		fmt.Printf("%-8s %s\n", signer.State, signer.PublicKey)
	}
	return nil
}

func setTrust(state string) func(*cobra.Command, []string) error {
	return func(command *cobra.Command, args []string) error {
		held, tools, err := ownerTools(command)
		if err != nil {
			return err
		}

		record, err := tools.Trust(held.me.PublicKey, args[0], state, "")
		if err != nil {
			return err
		}

		fmt.Printf("%s is %s\n", short(record.PublicKey), record.State)
		return nil
	}
}

// runInstalled runs one installed command. It answers false when no install
// holds that name, so arc can report an unknown command instead.
func runInstalled(name string, argv []string) (bool, error) {
	// The flags of arc itself come out of the line. Everything else belongs
	// to the capability, which names its own flags.
	command := installedCommand(name)
	rest, err := readArcFlags(command, name, argv)
	if err != nil {
		return true, err
	}

	held, err := open(command, true)
	if err != nil {
		return false, nil
	}

	tools, err := toolStore(command)
	if err != nil {
		return false, nil
	}

	record, err := tools.Get(held.me.PublicKey, name)
	if err != nil {
		return false, nil
	}

	seconds, _ := command.Flags().GetInt("timeout")
	ctx, cancel := deadline(seconds)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return true, err
	}
	defer connection.Close()

	peers := connection.Peers()

	saved, err := listStore(command)
	if err != nil {
		return true, err
	}

	context := toolbox.Context{
		Identity: held.me,
		Resolve: func(query string) ([][]byte, error) {
			// A name may stand for a list of this command.
			if members := saved.Members(name, query); len(members) > 0 {
				var keys [][]byte
				for _, member := range members {
					found, err := lookUp(ctx, held, connection, member)
					if err != nil {
						return nil, err
					}
					keys = append(keys, found...)
				}
				return keys, nil
			}
			return lookUp(ctx, held, connection, query)
		},
	}

	invocation, err := toolbox.Build(record, rest, context)
	if err != nil {
		return true, err
	}

	// A command may take its body from the standard input, from one of its
	// arguments, or from a file.
	if invocation.Command != nil {
		if input, ok := invocation.Command["input"].(map[string]any); ok && input["source"] == "stdin" {
			var body []byte

			if readsStdin(input, invocation.Values) {
				body, err = io.ReadAll(io.LimitReader(os.Stdin, client.MaxBodyBytes+1))
				if err != nil {
					return true, err
				}
			}

			invocation.Body, err = toolbox.RenderStdin(invocation.Command, invocation.Values, body, context)
			if err != nil {
				return true, err
			}
		}
	}

	provider, _ := record["provider"].(map[string]any)
	peer, err := hex.DecodeString(text(provider["public_key"]))
	if err != nil {
		return true, errors.New("the install names no citizen")
	}

	// The citizen must still serve the package that the owner accepted.
	signed, err := peers.Detail(ctx, peer, text(record["capability_id"]))
	if err != nil {
		return true, err
	}
	if signed["package_hash"] != record["package_hash"] {
		return true, fmt.Errorf(
			"%s serves another version now: install it again with arc install %s",
			text(provider["name"]), text(provider["public_key"]))
	}

	answer, err := peers.Request(ctx, peer, invocation.Meta, []byte(invocation.Body))
	if err != nil {
		return true, err
	}

	body := string(answer.Body)
	if raw, _ := command.Flags().GetBool("raw"); !raw {
		body = renderAnswer(command, held, name, invocation.Command, body)
	}

	os.Stdout.WriteString(body)
	if len(body) > 0 && !strings.HasSuffix(body, "\n") {
		fmt.Println()
	}
	return true, nil
}

// renderAnswer runs the filters that the capability names. The cache is a
// filter of this machine, and keeps a copy of each record.
func renderAnswer(command *cobra.Command, held *settings, name string, spec map[string]any, body string) string {
	filters := toolbox.OutputFilters(spec)
	if len(filters) == 0 {
		return body
	}

	if hex, _ := command.Flags().GetBool("hex"); hex {
		filters = without(filters, "petnames")
	}

	// A format of the caller stands in for the one that the capability names.
	if format, _ := command.Flags().GetString("format"); format != "" {
		filters = append(without(without(filters, "conversation"), "markdown"), format)
	}

	cache := &toolbox.Cache{Dir: held.keys.Dir}
	extra := map[string]func(string) string{
		"cache": func(text string) string {
			if _, err := cache.Keep(name, held.me, text); err != nil {
				fmt.Fprintf(os.Stderr, "the cache did not keep this answer: %v\n", err)
			}
			return text
		},
	}
	return toolbox.ApplyOutput(body, filters, held.me, extra)
}

// without returns the filters, less the one of that name.
func without(filters []string, name string) []string {
	out := filters[:0:0]
	for _, filter := range filters {
		if filter != name {
			out = append(out, filter)
		}
	}
	return out
}

// readsStdin says whether a command still needs the standard input. An
// argument or a file that carries the body stands in for it, and a terminal
// never does.
func readsStdin(input map[string]any, values map[string]any) bool {
	for _, field := range []string{"body", "file"} {
		if name := text(input[field]); name != "" {
			if value, held := values[name]; held && value != nil && value != "" {
				return false
			}
		}
	}

	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}

// readArcFlags takes the flags of arc out of the line, and returns what the
// capability reads. The name of the command goes as well.
func readArcFlags(command *cobra.Command, name string, argv []string) ([]string, error) {
	takesValue := map[string]bool{
		"--relay": true, "--relay-pubkey": true, "--key": true, "--store": true,
		"--timeout": true,
	}
	takesValue["--format"] = true
	noValue := map[string]bool{"--raw": true, "--hex": true}

	var rest []string
	seenName := false

	for index := 0; index < len(argv); index++ {
		argument := argv[index]

		if takesValue[argument] {
			if index+1 >= len(argv) {
				return nil, fmt.Errorf("the flag %s needs a value", argument)
			}

			index++
			if err := command.Flags().Set(strings.TrimPrefix(argument, "--"), argv[index]); err != nil {
				return nil, err
			}
			continue
		}

		if noValue[argument] {
			if err := command.Flags().Set(strings.TrimPrefix(argument, "--"), "true"); err != nil {
				return nil, err
			}
			continue
		}

		if !seenName && argument == name {
			seenName = true
			continue
		}
		rest = append(rest, argument)
	}
	return rest, nil
}

// lookUp reads one citizen: a key, a name of this machine, or a name that
// the relay answers.
func lookUp(ctx context.Context, held *settings, connection *client.Client, query string) ([][]byte, error) {
	if key, err := peerKey(query); err == nil {
		return [][]byte{key}, nil
	}

	plane := &control.Store{Dir: held.keys.Dir}
	if entries, err := plane.Resolve(query); err == nil {
		var keys [][]byte
		for _, entry := range entries {
			if key, err := entry.Key(); err == nil {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 {
			return keys, nil
		}
	}

	entries, err := connection.Resolve(ctx, query)
	if err != nil {
		return nil, err
	}

	var keys [][]byte
	for _, entry := range entries {
		if key, err := hex.DecodeString(text(entry["public_key"])); err == nil {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// installedCommand holds the flags that every installed command takes.
func installedCommand(name string) *cobra.Command {
	command := &cobra.Command{Use: name}

	command.Flags().String("relay", "", "the relay to use, as host:port")
	command.Flags().String("relay-pubkey", "", "the public key to pin for the relay")
	command.Flags().String("key", "", "the identity to use, by petname")
	command.Flags().String("store", "", "the directory of ARC")
	command.Flags().Int("timeout", 30, "how many seconds to wait for the answer")
	command.Flags().Bool("raw", false, "print the answer as the provider wrote it")
	command.Flags().Bool("hex", false, "keep the public keys, instead of writing petnames")
	command.Flags().String("format", "", "show the answer as conversation or as markdown")
	return command
}

func ownerTools(command *cobra.Command) (*settings, *toolbox.Store, error) {
	held, err := open(command, false)
	if err != nil {
		return nil, nil, err
	}

	tools, err := toolStore(command)
	if err != nil {
		return nil, nil, err
	}
	return held, tools, nil
}

func toolStore(command *cobra.Command) (*toolbox.Store, error) {
	if dir, _ := command.Flags().GetString("store"); dir != "" {
		return &toolbox.Store{Dir: dir}, nil
	}
	return toolbox.DefaultStore()
}

func showInstall(record map[string]any) {
	provider, _ := record["provider"].(map[string]any)
	fields, _ := record["capability"].(map[string]any)

	fmt.Printf("%s runs %s/%s [%s/%s]\n",
		text(record["command"]), text(provider["name"]), text(record["capability_id"]),
		text(fields["kind"]), text(fields["scheme"]))
	fmt.Printf("version %s (%s)\n", text(record["release_version"]), text(record["channel"]))
	fmt.Printf("signer  %s\n", short(text(record["signer_public_key"])))
	fmt.Printf("hash    %s\n", short(text(record["package_hash"])))
	fmt.Printf("run     %s\n", text(record["usage"]))
}

func text(value any) string {
	out, _ := value.(string)
	return out
}

func short(value string) string {
	if len(value) <= 16 {
		return value
	}
	return value[:16]
}
