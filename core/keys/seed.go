package keys

import (
	"errors"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip06"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/text/unicode/norm"
)

// SeedWords is the number of words in a new seed: 256 bits of entropy.
const SeedWords = 24

// NewSeed makes the words of a new seed, as BIP-39 defines. The words are
// the paper backup of a citizen key. Keep them on paper only.
func NewSeed() (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", err
	}
	return bip39.NewMnemonic(entropy)
}

// FromSeed derives the citizen key of a seed, as NIP-06 defines, at the path
// m/44'/1237'/0'/0/0. The passphrase is the optional passphrase of BIP-39;
// an empty passphrase is the default. FromSeed accepts 12 or 24 words, in
// any case and with any spaces between them, and refuses a word that is not
// in the English list or a bad checksum.
func FromSeed(words, passphrase string) (Key, error) {
	fields := strings.Fields(strings.ToLower(words))
	if len(fields) != 12 && len(fields) != SeedWords {
		return Key{}, errors.New("keys: a seed has 12 or 24 words")
	}
	for _, word := range fields {
		if _, ok := bip39.GetWordIndex(word); !ok {
			return Key{}, errors.New("keys: a word of the seed is not in the BIP-39 list")
		}
	}
	// BIP-39 hashes the words as one string with single spaces, so the
	// same words always give the same key.
	seed, err := bip39.NewSeedWithErrorChecking(strings.Join(fields, " "), norm.NFKD.String(passphrase))
	if err != nil {
		return Key{}, errors.New("keys: the checksum of the seed is wrong")
	}
	text, err := nip06.PrivateKeyFromSeed(seed)
	if err != nil {
		return Key{}, err
	}
	secret, err := nostr.SecretKeyFromHex(text)
	if err != nil {
		return Key{}, err
	}
	return FromSecret(secret), nil
}
