package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gezibash/arc/go/release"
	"github.com/spf13/cobra"
)

// arc reads its own releases from a citizen that serves them, and replaces
// its program with one that the publisher signed.
//
// The publisher and the channel are named in the settings of this machine,
// or on the command line. Nothing is taken from a name alone: the channel
// document carries a signature, and the archive carries a hash.

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
	command.Flags().String("provider", "", "the citizen that serves the releases, by public key")
	command.Flags().String("publisher", "", "the public key that signs the channel")
	command.Flags().String("channel", "stable", "the channel to read")
	command.Flags().String("program", "", "the program to replace (default: this one)")
	return command
}

func update(command *cobra.Command, apply bool) error {
	held, err := open(command, true)
	if err != nil {
		return err
	}

	provider, publisher, channel, err := updateTarget(command)
	if err != nil {
		return err
	}

	ctx, cancel := deadline(300)
	defer cancel()

	connection, err := held.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()

	peers := connection.Peers()

	document, err := release.FetchChannel(ctx, peers, provider, channel)
	if err != nil {
		return err
	}

	verified, err := release.Verify(document, release.Expect{Publisher: publisher, Channel: channel})
	if err != nil {
		return err
	}

	platform := release.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	newest, err := verified.Select(platform, version)

	if errors.Is(err, release.ErrNoRelease) {
		fmt.Printf("arc %s is the newest release of %s for %s/%s\n",
			version, channel, platform.OS, platform.Arch)
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Printf("%s names arc %s for %s/%s\n", channel, newest.Version, platform.OS, platform.Arch)
	fmt.Printf("  build   %s\n", newest.Build)
	fmt.Printf("  hash    %s\n", newest.Archive().SHA256)
	fmt.Printf("  size    %d bytes\n", newest.Archive().Size)

	if !newest.Eligible {
		return errors.New("the channel says that this release cannot be installed")
	}
	if !apply {
		fmt.Println("\nrun arc update apply to install it")
		return nil
	}

	program, err := programPath(command)
	if err != nil {
		return err
	}

	archive, err := release.Download(ctx, peers, provider, newest.Archive(), progress)
	if err != nil {
		return err
	}
	fmt.Println()

	binary, err := release.Unpack(archive, filepath.Base(program))
	if err != nil {
		return err
	}
	if err := release.Replace(program, binary, newest.Version); err != nil {
		return err
	}

	fmt.Printf("arc %s is installed at %s\n", newest.Version, program)
	fmt.Printf("the release it replaced stands at %s.previous\n", program)
	return nil
}

// updateTarget reads who serves the releases, who signs them, and which
// channel to read.
func updateTarget(command *cobra.Command) (provider, publisher []byte, channel string, err error) {
	name, _ := command.Flags().GetString("provider")
	if name == "" {
		name = os.Getenv("ARC_RELEASES")
	}
	if name == "" {
		return nil, nil, "", errors.New("name the citizen that serves the releases with --provider, or ARC_RELEASES")
	}

	if provider, err = peerKey(name); err != nil {
		return nil, nil, "", err
	}

	signer, _ := command.Flags().GetString("publisher")
	if signer == "" {
		signer = os.Getenv("ARC_RELEASE_PUBLISHER")
	}
	if signer == "" {
		// The citizen that serves a channel is the one that signs it, unless
		// the operator names another.
		signer = name
	}

	if publisher, err = hex.DecodeString(strings.ToLower(signer)); err != nil || len(publisher) != 32 {
		return nil, nil, "", errors.New("the publisher is named by 64 characters of hex")
	}

	channel, _ = command.Flags().GetString("channel")
	if channel == "" {
		channel = "stable"
	}
	return provider, publisher, channel, nil
}

// programPath is the file to replace: the one that runs, or the one that the
// operator named.
func programPath(command *cobra.Command) (string, error) {
	if given, _ := command.Flags().GetString("program"); given != "" {
		return filepath.Abs(given)
	}

	running, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(running)
}

// progress shows how much of an archive has arrived.
func progress(read, total int64) {
	fmt.Printf("\r  read    %d of %d bytes", read, total)
}
