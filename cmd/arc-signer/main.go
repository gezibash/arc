// Command arc-signer is the program signer of ARC: it holds the citizen key
// of one identity, and answers requests on its standard input, as
// docs/devices/SPEC.md, section 3.8, defines. arc starts it; a person runs
// it only to make, recover, import or seal a key.
//
//	arc-signer stdio | gen | recover | import | encrypt [--dir <identity>]
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "arc-signer: "+terminal.EscapeLine(err.Error()))
		os.Exit(1)
	}
}

func root() *cobra.Command {
	command := &cobra.Command{
		Use:           "arc-signer",
		Short:         "Hold the citizen key of one identity, and answer for it",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	command.PersistentFlags().String("dir", ".", "the folder of the identity; the key is in signer/key in it")
	gen := &cobra.Command{
		Use:   "gen",
		Short: "Make a key from a new 24-word seed, and show the words once",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			words, err := keys.NewSeed()
			if err != nil {
				return err
			}
			passphrase, err := seedPassphrase(command, true)
			if err != nil {
				return err
			}
			k, err := keys.FromSeed(words, passphrase)
			if err != nil {
				return err
			}
			if err := keep(command, k); err != nil {
				return err
			}
			showSeed(command.ErrOrStderr(), words, passphrase != "")
			fmt.Fprintln(command.OutOrStdout(), k.Public.Hex())
			return nil
		},
	}
	gen.Flags().Bool("encrypt", false, "seal the key with a passphrase, as NIP-49 defines")
	gen.Flags().Bool("seed-passphrase", false, "add a BIP-39 passphrase to the seed")

	recoverSeed := &cobra.Command{
		Use:   "recover",
		Short: "Make the key again from the words of its seed, read on standard input",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			words, err := readSeed(command)
			if err != nil {
				return err
			}
			passphrase, err := seedPassphrase(command, false)
			if err != nil {
				return err
			}
			k, err := keys.FromSeed(words, passphrase)
			if err != nil {
				return err
			}
			if err := keep(command, k); err != nil {
				return err
			}
			fmt.Fprintln(command.OutOrStdout(), k.Public.Hex())
			return nil
		},
	}
	recoverSeed.Flags().Bool("encrypt", false, "seal the key with a passphrase, as NIP-49 defines")
	recoverSeed.Flags().Bool("seed-passphrase", false, "the seed has a BIP-39 passphrase")

	importKey := &cobra.Command{
		Use:   "import",
		Short: "Take a key as hex, nsec or ncryptsec, from --from or standard input",
		Long: "arc-signer keeps the key as it comes: an ncryptsec stays sealed. It\n" +
			"prints the public key. For an ncryptsec it needs --public, or the\n" +
			"passphrase.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			from, _ := command.Flags().GetString("from")
			var text string
			if from != "" {
				var err error
				if text, err = keyfile.Read(from); err != nil {
					return err
				}
			} else {
				body, err := io.ReadAll(io.LimitReader(command.InOrStdin(), 4096))
				if err != nil {
					return err
				}
				text = strings.TrimSpace(string(body))
			}
			if text == "" {
				return errors.New("give the key on standard input, or with --from")
			}
			public, err := publicOf(command, text)
			if err != nil {
				return err
			}
			if err := write(command, text); err != nil {
				return err
			}
			fmt.Fprintln(command.OutOrStdout(), public)
			return nil
		},
	}
	importKey.Flags().String("from", "", "the file that holds the key")
	importKey.Flags().String("public", "", "the public key of an ncryptsec, in hex, so no passphrase is needed")

	encrypt := &cobra.Command{
		Use:   "encrypt",
		Short: "Seal the key with a passphrase, as NIP-49 defines",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			k, err := keyfile.Load(keyPath(command))
			if err != nil {
				return err
			}
			sealed, err := newPassphrase(k)
			if err != nil {
				return err
			}
			if err := write(command, sealed); err != nil {
				return err
			}
			fmt.Fprintln(command.OutOrStdout(), "the key is sealed; arc-signer asks for the passphrase, or reads ARC_PASSPHRASE")
			return nil
		},
	}

	stdio := &cobra.Command{
		Use:   "stdio",
		Short: "Answer one request for each JSON line on standard input",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			k, err := load(command)
			if err != nil {
				return err
			}
			return serve(command, k, command.InOrStdin(), command.OutOrStdout())
		},
	}
	command.AddCommand(gen, recoverSeed, importKey, encrypt, stdio)
	return command
}

// keyPath is the file of the key: signer/key in the folder of the identity.
func keyPath(command *cobra.Command) string {
	dir, _ := command.Flags().GetString("dir")
	return filepath.Join(dir, "signer", "key")
}

// keep writes a new key, sealed when --encrypt asks for it.
func keep(command *cobra.Command, k keys.Key) error {
	text := k.Secret.Hex()
	if seal, _ := command.Flags().GetBool("encrypt"); seal {
		var err error
		if text, err = newPassphrase(k); err != nil {
			return err
		}
	}
	return write(command, text)
}

// write keeps text as the key, readable only by this user.
func write(command *cobra.Command, text string) error {
	path := keyPath(command)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return keyfile.Write(path, text)
}

// publicOf is the public key of the key text, in hex.
func publicOf(command *cobra.Command, text string) (string, error) {
	if !strings.HasPrefix(text, "ncryptsec1") {
		k, err := keys.Parse(text)
		if err != nil {
			return "", err
		}
		return k.Public.Hex(), nil
	}
	if given, _ := command.Flags().GetString("public"); given != "" {
		return given, nil
	}
	passphrase, err := askPassphrase("passphrase of the key: ")
	if err != nil {
		return "", err
	}
	k, err := keys.Decrypt(text, passphrase)
	if err != nil {
		return "", err
	}
	return k.Public.Hex(), nil
}

// load reads the key, and asks for the passphrase of a sealed one.
func load(command *cobra.Command) (keys.Key, error) {
	text, err := keyfile.Read(keyPath(command))
	if err != nil {
		return keys.Key{}, err
	}
	if strings.HasPrefix(text, "ncryptsec1") {
		passphrase, err := askPassphrase("passphrase of the key: ")
		if err != nil {
			return keys.Key{}, err
		}
		return keys.Decrypt(text, passphrase)
	}
	return keys.Parse(text)
}

// serve answers each request line, in order, until the input ends.
func serve(command *cobra.Command, k keys.Key, in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 64<<10)
	w := bufio.NewWriter(out)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var req keys.Request
			response := keys.Response{Error: "the line is not a request"}
			if jsonErr := json.Unmarshal(line, &req); jsonErr == nil {
				response = k.Answer(command.Context(), req)
			}
			encoded, mErr := json.Marshal(response)
			if mErr != nil {
				return mErr
			}
			if _, wErr := w.Write(append(encoded, '\n')); wErr != nil {
				return wErr
			}
			if fErr := w.Flush(); fErr != nil {
				return fErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
