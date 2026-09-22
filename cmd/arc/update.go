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

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/internal/canonical"
	"github.com/gezibash/arc/release"
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
	return command
}

// releaseCaller asks one provider for releases with live calls.
type releaseCaller struct {
	sess     *session
	provider nostr.PubKey
}

func (r releaseCaller) Request(ctx context.Context, body []byte) ([]byte, error) {
	request := call.Request{Capability: "releases", Method: "RAW", Path: "/releases", Body: string(body)}
	reply, _, _, err := liveCall(ctx, r.sess, r.provider, request, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if reply.Err != "" {
		return nil, fmt.Errorf("the releases provider refused: %s", reply.Err)
	}
	return []byte(reply.Body), nil
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
	env := &cliEnv{installs: installs}
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
	defer sess.close()
	dir, err := home(command)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Minute)
	defer cancel()
	source := releaseCaller{sess: sess, provider: provider}
	checkpoint := &release.Checkpoint{Dir: dir}
	platform := release.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	newest, err := release.Newest(ctx, source, checkpoint, publisher[:], channel, platform, version)
	if errors.Is(err, release.ErrNoRelease) {
		fmt.Printf("arc %s is the newest release of %s for %s/%s\n", version, channel, platform.OS, platform.Arch)
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Printf("%s names arc %s for %s/%s\n", channel, newest.Version, platform.OS, platform.Arch)
	fmt.Printf("  build   %s\n", newest.Build)
	fmt.Printf("  hash    %s\n", newest.Archive().SHA256)
	fmt.Printf("  size    %d bytes\n", newest.Archive().Size)
	if !apply {
		fmt.Println("\nrun arc update apply to install it")
		return nil
	}

	program, _ := flags.GetString("program")
	if program == "" {
		running, err := os.Executable()
		if err != nil {
			return err
		}
		if program, err = filepath.EvalSymlinks(running); err != nil {
			return err
		}
	} else if program, err = filepath.Abs(program); err != nil {
		return err
	}
	progress := func(read, total int64) { fmt.Printf("\r  read    %d of %d bytes", read, total) }
	if err := release.Apply(ctx, source, newest, program, progress); err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("arc %s is installed at %s\n", newest.Version, program)
	fmt.Printf("the release it replaced stands at %s.previous\n", program)
	return nil
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
			"writes <root>/channels/<channel>.json. See docs/updates/PUBLISHING.md.",
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
			if id.remote {
				return errRemote("signing a release")
			}
			signed, err := release.Sign(id.key.Secret, unsigned)
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
			fmt.Printf("signed %s, sequence %v, as %s\n", target, unsigned["sequence"], id.key.Public.Hex())
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
	now, _ := current["sequence"].(json.Number).Int64()
	next, _ := unsigned["sequence"].(json.Number).Int64()
	if next <= now {
		return fmt.Errorf("the sequence must be above %d, the sequence of %s", now, target)
	}
	return nil
}
