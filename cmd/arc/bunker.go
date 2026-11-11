package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/runtime/bunker"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func bunkerCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "bunker",
		Short: "Sign for others through a relay, as NIP-46 defines",
		Long: "arc prints a bunker:// URI. A machine that uses it, such as an agent,\n" +
			"signs with this citizen's key and never holds it. With --allow-kind,\n" +
			"the bunker signs only those kinds, and authentication for relays. By\n" +
			"default it decrypts only what the owner sealed to themselves, which\n" +
			"the journal and the files need; mail needs --decrypt all. When a\n" +
			"device list exists, the bunker admits only its devices: see\n" +
			"arc keys devices.",
		Args: cobra.NoArgs,
		RunE: serveBunker,
	}
	command.Flags().String("relay", "", "the relay that carries the requests")
	command.Flags().StringArray("allow-kind", nil, "a kind that the bunker signs; with none, it signs every kind")
	command.Flags().String("decrypt", "self", "what the bunker decrypts: none, self (drafts and the keyed root), or all (needed for mail)")
	return command
}

func serveBunker(command *cobra.Command, _ []string) error {
	url, p, err := bunkerFlags(command)
	if err != nil {
		return err
	}
	dir, err := home(command)
	if err != nil {
		return err
	}
	id, err := loadIdentity(command.Context(), dir)
	if err != nil {
		return err
	}
	defer id.done()
	remote, ok := id.signer.(keys.Remote)
	if id.remote || !ok {
		return errRemote("a bunker")
	}
	secret, err := bunker.Secret(dir)
	if err != nil {
		return err
	}

	// A machine that signs through this bunker reads the keyed root from
	// the bunker's relay.
	sess, err := open(command)
	if err != nil {
		return err
	}
	g := bunker.NewGuard(secret)
	err = bunker.Prepare(command.Context(), sess, url, g, os.Stderr)
	sess.Close()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	pk := id.key.Public

	// With a device list, the secret admits nobody, so the URI leaves it out.
	if g.Listed() {
		fmt.Fprintf(command.OutOrStdout(), "bunker://%s?relay=%s\n", pk.Hex(), terminal.EscapeLine(url))
	} else {
		fmt.Fprintf(command.OutOrStdout(), "bunker://%s?relay=%s&secret=%s\n", pk.Hex(), terminal.EscapeLine(url), secret)
	}
	go bunker.WatchDevices(ctx, relay.Relay{URL: url, Signer: remote}, remote, g, log)
	p.Admit = g.Refusal
	return bunker.Watch(ctx, url, pk, remote, p, g.Authorize, log, os.Stderr)
}

// bunkerFlags reads the relay and the policy of arc bunker.
func bunkerFlags(command *cobra.Command) (string, bunker.Policy, error) {
	url, _ := command.Flags().GetString("relay")
	if url == "" {
		return "", bunker.Policy{}, errors.New("name the relay with --relay")
	}
	// A bad URL fails now, and not at each try to reach the relay.
	if err := checkRelayURL(url); err != nil {
		return "", bunker.Policy{}, fmt.Errorf("--relay: %w", err)
	}
	decrypt, _ := command.Flags().GetString("decrypt")
	if !slices.Contains([]string{"none", "self", "all"}, decrypt) {
		return "", bunker.Policy{}, fmt.Errorf("--decrypt is none, self or all, not %q", decrypt)
	}
	var allowed []nostr.Kind
	texts, _ := command.Flags().GetStringArray("allow-kind")
	for _, text := range texts {
		n, err := strconv.Atoi(text)
		if err != nil {
			return "", bunker.Policy{}, fmt.Errorf("--allow-kind %q is not a kind", text)
		}
		allowed = append(allowed, nostr.Kind(n))
	}
	return url, bunker.Policy{Kinds: allowed, Decrypt: decrypt}, nil
}
