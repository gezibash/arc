package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gezibash/arc/internal/canonical"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/release"
	"github.com/spf13/cobra"
)

// arc reads its own releases from a citizen that serves them, and replaces
// its program with one that the publisher signed. See docs/updates/SPEC.md.

func updateCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "update",
		Short: "Read the release channel, and replace this program",
		Long: "Without a subcommand, arc reads the channel and says what it\n" +
			"would do. arc update apply replaces the program.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error { return update(command, false) },
	}
	command.AddCommand(
		withUpdateFlags(&cobra.Command{
			Use: "check", Short: "Say which release the channel names",
			Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error { return update(command, false) },
		}),
		withUpdateFlags(&cobra.Command{
			Use: "apply", Short: "Replace this program with the release that the channel names",
			Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error { return update(command, true) },
		}),
	)
	return withUpdateFlags(command)
}

func withUpdateFlags(command *cobra.Command) *cobra.Command {
	command.Flags().String("provider", "", "the citizen that serves the releases (ARC_RELEASES)")
	command.Flags().String("publisher", "", "the key that signs the channel (ARC_RELEASE_PUBLISHER, default the provider)")
	command.Flags().String("channel", "stable", "the channel to read")
	command.Flags().String("program", "", "the program to replace (default: this one)")
	command.Flags().String("path", "", "the path of the archive: auto, direct, turn or session (default: arc path)")
	return command
}

func update(command *cobra.Command, apply bool) error {
	flags := command.Flags()
	providerText, _ := flags.GetString("provider")
	if providerText == "" {
		providerText = os.Getenv("ARC_RELEASES")
	}
	if providerText == "" {
		return errors.New("name the citizen that serves the releases with --provider, or ARC_RELEASES")
	}
	publisherText, _ := flags.GetString("publisher")
	if publisherText == "" {
		publisherText = os.Getenv("ARC_RELEASE_PUBLISHER")
	}
	if publisherText == "" {
		// The citizen that serves a channel is the one that signs it, unless
		// the operator names another.
		publisherText = providerText
	}
	channel, _ := flags.GetString("channel")

	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	env := &citizen.Environment{Installs: installs}
	provider, err := env.ResolveKey(command.Context(), providerText)
	if err != nil {
		return fmt.Errorf("--provider: %w", err)
	}
	publisher, err := env.ResolveKey(command.Context(), publisherText)
	if err != nil {
		return fmt.Errorf("--publisher: %w", err)
	}

	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	dir, err := home(command)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Minute)
	defer cancel()
	pipes, err := dialConfig(command)
	if err != nil {
		return err
	}
	program, _ := flags.GetString("program")
	return release.Update{
		Session: sess, Installs: installs, Provider: provider, Publisher: publisher,
		Channel: channel, Dir: dir, Pipes: pipes,
		Platform: release.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}, Version: version,
		Apply: apply, Program: program, Out: command.OutOrStdout(), Errs: command.ErrOrStderr(),
	}.Run(ctx)
}

func releaseCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "release",
		Short: "Publish signed release channels",
	}
	sign := &cobra.Command{
		Use:   "sign <unsigned channel.json>",
		Short: "Sign a channel, and write it for the releases provider",
		Long: "arc checks the size and the hash of each archive that the channel\n" +
			"names, in <root>/blobs/<sha256>.tar.gz. It then signs the channel with\n" +
			"the chosen identity, which must be the publisher of the channel, and\n" +
			"writes <root>/channels/<channel>.json. See apps/releases/README.md.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			root, _ := command.Flags().GetString("root")
			if root == "" {
				return errors.New("name the directory of the releases provider with --root")
			}
			body, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			value, err := canonical.Decode(body)
			if err != nil {
				return fmt.Errorf("%s is not a channel document: %w", args[0], err)
			}
			unsigned, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s is not a channel document", args[0])
			}
			if err := checkBlobs(root, unsigned); err != nil {
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
			if id.holder == nil {
				return errRemote("signing a release")
			}
			signed, err := release.Sign(command.Context(), id.holder, unsigned)
			if err != nil {
				return err
			}
			encoded, err := json.MarshalIndent(signed, "", "  ")
			if err != nil {
				return err
			}

			// Replace the channel file whole, so the provider never serves a
			// part of it.
			name, _ := unsigned["channel"].(string)
			target := filepath.Join(root, "channels", name+".json")
			if err := checkSuccessor(target, unsigned); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			temporary := target + ".new"
			if err := os.WriteFile(temporary, append(encoded, '\n'), 0o644); err != nil {
				return err
			}
			if err := os.Rename(temporary, target); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "signed %s, sequence %v, as %s\n", target, unsigned["sequence"], id.key.Public.Hex())
			return nil
		},
	}
	sign.Flags().String("root", "", "the directory of the releases provider (RELEASES_ROOT)")
	command.AddCommand(sign)
	return command
}

// checkBlobs checks that each archive that the channel names is in the
// blobs of the provider, with its size and its hash.
func checkBlobs(root string, unsigned map[string]any) error {
	list, _ := unsigned["releases"].([]any)
	for _, item := range list {
		fields, _ := item.(map[string]any)
		artifacts := []map[string]any{fields}
		if install, ok := fields["install"].(map[string]any); ok {
			artifacts = append(artifacts, install)
		}
		for _, artifact := range artifacts {
			digest, _ := artifact["sha256"].(string)
			size, _ := artifact["size"].(json.Number)
			want, err := size.Int64()
			if err != nil || len(digest) != 64 {
				return fmt.Errorf("release %v names no size and sha256", fields["version"])
			}
			path := filepath.Join(root, "blobs", digest+".tar.gz")
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("release %v: %w", fields["version"], err)
			}
			got := sha256.Sum256(data)
			if int64(len(data)) != want || hex.EncodeToString(got[:]) != digest {
				return fmt.Errorf("release %v: %s does not match its size and hash", fields["version"], path)
			}
		}
	}
	return nil
}

// checkSuccessor refuses a channel that does not follow the one that the
// provider serves now: another publisher, or a sequence that does not
// increase. It also refuses a channel file that is a link.
func checkSuccessor(target string, unsigned map[string]any) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", target)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	value, err := canonical.Decode(body)
	current, _ := value.(map[string]any)
	if err != nil || current == nil {
		return fmt.Errorf("%s is not a channel document", target)
	}
	if current["publisher"] != unsigned["publisher"] {
		return fmt.Errorf("%s has another publisher: a new publisher needs an explicit trust transition", target)
	}
	nowNumber, _ := current["sequence"].(json.Number)
	now, err := nowNumber.Int64()
	if err != nil {
		return fmt.Errorf("%s has no whole-number sequence", target)
	}
	nextNumber, _ := unsigned["sequence"].(json.Number)
	next, err := nextNumber.Int64()
	if err != nil {
		return fmt.Errorf("the new channel document has no whole-number sequence")
	}
	if next <= now {
		return fmt.Errorf("the sequence must be above %d, the sequence of %s", now, target)
	}
	return nil
}
