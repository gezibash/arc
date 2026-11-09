package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gezibash/arc/core/keys"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// keepSeedKey keeps a key that a seed derives, sealed with NIP-49 when
// --encrypt asks for it. The key file format does not change.
func keepSeedKey(command *cobra.Command, root string, k keys.Key) (keys.Key, error) {
	text := k.Secret.Hex()
	if encrypt, _ := command.Flags().GetBool("encrypt"); encrypt {
		var err error
		if text, err = newPassphrase(k); err != nil {
			return keys.Key{}, err
		}
	}
	_, why, err := rootSource(command)
	if err != nil {
		return keys.Key{}, err
	}
	return addCitizen(command.Context(), root, why, text, &k)
}

// seedPassphrase returns the BIP-39 passphrase when --seed-passphrase asks
// for one, and an empty passphrase when it does not. A new seed asks twice,
// because a forgotten passphrase loses the key.
func seedPassphrase(command *cobra.Command, confirm bool) (string, error) {
	if want, _ := command.Flags().GetBool("seed-passphrase"); !want {
		return "", nil
	}
	first, err := askSecret("ARC_SEED_PASSPHRASE", "passphrase of the seed: ")
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", errors.New("the passphrase of the seed is empty")
	}
	if confirm && os.Getenv("ARC_SEED_PASSPHRASE") == "" {
		again, err := askSecret("ARC_SEED_PASSPHRASE", "the passphrase of the seed again: ")
		if err != nil {
			return "", err
		}
		if again != first {
			return "", errors.New("the two passphrases of the seed differ")
		}
	}
	return first, nil
}

// readSeed reads the words of a seed from standard input. On a terminal it
// asks for them first.
func readSeed(command *cobra.Command) (string, error) {
	in := command.InOrStdin()
	if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(command.ErrOrStderr(), "the words of the seed, then Enter: ")
		line, err := bufio.NewReader(file).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return line, nil
	}
	body, err := io.ReadAll(io.LimitReader(in, 4096))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(body)) == "" {
		return "", errors.New("give the words of the seed on standard input")
	}
	return string(body), nil
}

// showSeed writes the words once, numbered, with a warning.
func showSeed(w io.Writer, words string, withPassphrase bool) {
	fmt.Fprintln(w, "Write these 24 words on paper, in order. They are the only backup of")
	fmt.Fprintln(w, "this identity. arc does not store them and does not show them again.")
	fmt.Fprintln(w, "Anyone who reads them has the key. Never type them into a chat or a website.")
	if withPassphrase {
		fmt.Fprintln(w, "The words need the passphrase too. Without it, they give another key.")
	}
	fmt.Fprintln(w)
	for i, word := range strings.Fields(words) {
		fmt.Fprintf(w, "%2d. %s\n", i+1, word)
	}
	fmt.Fprintln(w)
}

// newPassphrase asks for a passphrase twice, and seals the key with it.
func newPassphrase(k keys.Key) (string, error) {
	first, err := askPassphrase("new passphrase: ")
	if err != nil {
		return "", err
	}
	if os.Getenv("ARC_PASSPHRASE") == "" {
		again, err := askPassphrase("the passphrase again: ")
		if err != nil {
			return "", err
		}
		if again != first {
			return "", errors.New("the two passphrases differ")
		}
	}
	return keys.Encrypt(k, first)
}
