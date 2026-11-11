package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr/nip46"
	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/adapters/signer/program"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/homedir"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// Each identity of this machine has its own directory, <root>/citizens/<name>,
// because the store, the relays and the installs belong to one key. The file
// <root>/default names the identity that commands use when --key does not.

func keysCommand() *cobra.Command {
	command := &cobra.Command{Use: "keys", Short: "Make, add, list and pick identities"}

	command.AddCommand(keysGenCommand(), keysRecoverSeedCommand(), keysAddCommand(), keysListCommand(), keysUseCommand(), keysRemoveCommand(), keysEncryptCommand(), bunkerCommand(), deviceCommand(), devicesCommand(), accountCommand(), storageKeysCommand(), rootsCommand(), sealCommand(), unsealCommand())
	return command
}

func keysGenCommand() *cobra.Command {
	gen := &cobra.Command{
		Use:   "gen",
		Short: "Make an identity from a new 24-word seed, and make it the default when there is none",
		Long: "arc-signer, the program signer of the identity, makes a seed of 24\n" +
			"words, as BIP-39 and NIP-06 define, and derives the key from it. It keeps\n" +
			"the key in signer/key in the folder of the identity; arc never reads it.\n" +
			"arc-signer shows the words once, on standard error, and never stores them. Write them on paper: arc keys recover makes the same key\n" +
			"from them. Anyone who reads the words has the key.\n\n" +
			"--seed-passphrase adds a BIP-39 passphrase. The words and the passphrase\n" +
			"together give the key; without the passphrase, the words give another\n" +
			"key. arc reads it from ARC_SEED_PASSPHRASE, or asks on the terminal.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return makeSigned(command, "gen")
		},
	}
	gen.Flags().Bool("encrypt", false, "seal the key file with a passphrase, as NIP-49 defines")
	gen.Flags().Bool("seed-passphrase", false, "add a BIP-39 passphrase to the seed")
	return gen
}

func keysRecoverSeedCommand() *cobra.Command {
	recoverSeed := &cobra.Command{
		Use:   "recover",
		Short: "Make an identity again from the words of its seed",
		Long: "arc reads 12 or 24 words of a BIP-39 seed from standard input, or asks\n" +
			"on the terminal, and derives the key as NIP-06 defines. arc never takes\n" +
			"the words as arguments, because the shell history and ps show arguments.\n" +
			"--seed-passphrase reads the BIP-39 passphrase of the seed from\n" +
			"ARC_SEED_PASSPHRASE, or asks on the terminal.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return makeSigned(command, "recover")
		},
	}
	recoverSeed.Flags().Bool("encrypt", false, "seal the key file with a passphrase, as NIP-49 defines")
	recoverSeed.Flags().Bool("seed-passphrase", false, "the seed has a BIP-39 passphrase")
	return recoverSeed
}

func keysAddCommand() *cobra.Command {
	add := &cobra.Command{
		Use:   "add [bunker://...]",
		Short: "Add an identity that exists already",
		Long: "arc-signer takes the key from standard input: 64 characters of hex,\n" +
			"an nsec, or an ncryptsec of NIP-49. arc never reads it. With a bunker://\n" +
			"URI, the identity signs through a remote signer, as NIP-46 defines, and\n" +
			"this machine holds no secret key. An agent uses this: its owner runs\n" +
			"arc keys bunker.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				// arc-signer takes the key from standard input: arc never reads it.
				k, err := addSigned(command, "import")
				if err != nil {
					return err
				}
				fmt.Fprintf(command.OutOrStdout(), "%s\n%s\n", k.Name(), k.Public.Hex()) //arc:raw-ok the name of a key of this machine
				return nil
			}
			if !nip46.IsValidBunkerURL(args[0]) {
				return errors.New("that is not a bunker:// URI; give a secret key on standard input")
			}
			text := args[0]
			_, why, err := rootSource(command)
			if err != nil {
				return err
			}
			k, err := addCitizen(command.Context(), root, why, text, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "%s\n%s\n", k.Name(), k.Public.Hex()) //arc:raw-ok the name of a key of this machine
			if strings.HasPrefix(text, "bunker://") {
				// The first token lets this device read the sealed data of
				// the citizen on a relay with no path to the signer.
				sess, err := openDir(command, filepath.Join(homedir.CitizensDir(root), k.Name()))
				if err != nil {
					return err
				}
				defer sess.Close()
				if err := sess.RenewToken(command.Context()); err != nil {
					fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("arc: this device has no token for the relays yet, so it reads sealed data through the signer: %v", citizen.SignerOnly(err))))
				}
			}
			return nil
		},
	}
	return add
}

func keysListCommand() *cobra.Command {
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List the identities of this machine", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			root, why, err := rootSource(command)
			if err != nil {
				return err
			}
			all, err := listCitizens(root)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return homedir.UnreadableIdentities(root, why, err)
			}
			if err != nil {
				return err
			}
			if len(all) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no identities: make one with arc keys gen")
				return nil
			}
			body, err := os.ReadFile(homedir.DefaultPath(root))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return homedir.UnreadableDefault(root, why, err)
			}
			active := strings.TrimSpace(string(body))
			for _, c := range all {
				mark := " "
				if c.name == active {
					mark = "*"
				}
				fmt.Fprintf(command.OutOrStdout(), "%s %s  %s\n", mark, terminal.EscapeLine(c.name), terminal.EscapeLine(c.public))
			}
			return nil
		},
	}
	return list
}

func keysUseCommand() *cobra.Command {
	use := &cobra.Command{
		Use: "use <name>", Short: "Make one identity the default", Args: cobra.ExactArgs(1),
		Long: "Give the raw name of the identity, quoted for the shell. arc keys list\n" +
			"shows a name with an escape for each character that could change the\n" +
			"terminal, such as \\n for a newline: that escape is not the name.",
		RunE: func(command *cobra.Command, args []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(homedir.CitizensDir(root), args[0])); err != nil || !validName(args[0]) {
				return fmt.Errorf("no identity \"%s\" on this machine: see arc keys list", args[0])
			}
			if err := os.WriteFile(homedir.DefaultPath(root), []byte(args[0]+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "the default is %s\n", terminal.EscapeLine(args[0]))
			return nil
		},
	}
	return use
}

func keysRemoveCommand() *cobra.Command {
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove one identity from this machine",
		Long: "This removes the key, and the store, the relays and the installs of\n" +
			"the identity. A key that no other machine holds is lost for good.\n\n" +
			"Give the raw name of the identity, quoted for the shell. arc keys list\n" +
			"shows a name with an escape for each character that could change the\n" +
			"terminal, such as \\n for a newline: that escape is not the name.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			name := args[0]
			dir := filepath.Join(homedir.CitizensDir(root), name)
			if _, err := os.Stat(dir); err != nil || !validName(name) {
				return fmt.Errorf("no identity \"%s\" on this machine: see arc keys list", name)
			}
			if yes, _ := command.Flags().GetBool("yes"); !yes {
				fmt.Fprintf(command.OutOrStdout(), "Remove %s, its key and its store? [y/N] ", terminal.EscapeLine(name))
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(answer)) != "y" {
					return errors.New("nothing was removed")
				}
			}
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			if defaultName(root) == name {
				if err := os.Remove(homedir.DefaultPath(root)); err != nil {
					return err
				}
			}
			fmt.Fprintf(command.OutOrStdout(), "removed %s\n", terminal.EscapeLine(name))
			return nil
		},
	}
	remove.Flags().Bool("yes", false, "remove without asking")
	return remove
}

func keysEncryptCommand() *cobra.Command {
	encrypt := &cobra.Command{
		Use: "encrypt", Short: "Seal the key with a passphrase, as NIP-49 defines", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir, err := home(command)
			if err != nil {
				return err
			}
			text, err := keyfile.Read(keyPath(dir))
			if err != nil {
				return err
			}
			if strings.HasPrefix(text, "bunker://") {
				return errors.New("the key of this identity is on a remote signer; seal it there")
			}
			if !strings.HasPrefix(text, program.Prefix) {
				if err := moveToSigner(command.Context(), dir); err != nil {
					return err
				}
			}
			path, err := program.Find(signerProgram)
			if err != nil {
				return err
			}
			run := exec.CommandContext(command.Context(), path, "encrypt", "--dir", dir)
			run.Stdin, run.Stderr = command.InOrStdin(), command.ErrOrStderr()
			if err := run.Run(); err != nil {
				return fmt.Errorf("arc-signer encrypt: %w", err)
			}
			fmt.Fprintln(command.OutOrStdout(), "the key is sealed; arc-signer asks for the passphrase, or reads ARC_PASSPHRASE")
			return nil
		},
	}
	return encrypt
}

func whoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use: "whoami", Short: "Show the identity that commands use here", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir, source, err := chosen(command)
			if err != nil {
				return err
			}
			public, err := os.ReadFile(publicPath(dir))
			if err != nil {
				return err
			}
			root, why, err := rootSource(command)
			if err != nil {
				return err
			}
			// A folder can hold any character, such as a newline in HOME.
			fmt.Fprintf(command.OutOrStdout(), "%s\n%s\nchosen by %s\nhome %s, chosen by %s\n", terminal.EscapeLine(filepath.Base(dir)), terminal.EscapeLine(strings.TrimSpace(string(public))), source, terminal.EscapeLine(root), terminal.EscapeLine(why))
			return nil
		},
	}
}

// makeSigned makes an identity with arc-signer gen or recover, which show
// the seed and ask for the passphrases themselves.
func makeSigned(command *cobra.Command, what string) error {
	args := []string{what}
	for _, flag := range []string{"encrypt", "seed-passphrase"} {
		if on, _ := command.Flags().GetBool(flag); on {
			args = append(args, "--"+flag)
		}
	}
	k, err := addSigned(command, args...)
	if err != nil {
		return err
	}
	root, err := rootDir(command)
	if err != nil {
		return err
	}
	fmt.Fprintf(command.ErrOrStderr(), "wrote the identity in %s\n", terminal.EscapeLine(root))
	fmt.Fprintf(command.OutOrStdout(), "%s\n%s\n", k.Name(), k.Public.Hex()) //arc:raw-ok the name of a key of this machine
	return nil
}
