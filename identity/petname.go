package identity

import (
	"encoding/hex"

	"lukechampine.com/blake3"
)

// Name returns the full petname of a public key, for example
// "bold-einstein-3a7f0bc1". The same key always gives the same name.
func Name(publicKey []byte) string {
	adjective, noun, suffix, ok := parts(publicKey)
	if !ok {
		return ""
	}
	return adjective + "-" + noun + "-" + suffix
}

// ShortName returns the short petname of a public key, for example
// "bold-einstein".
func ShortName(publicKey []byte) string {
	adjective, noun, _, ok := parts(publicKey)
	if !ok {
		return ""
	}
	return adjective + "-" + noun
}

// Name returns the full petname of this identity.
func (id *Identity) Name() string { return Name(id.PublicKey) }

// ShortName returns the short petname of this identity.
func (id *Identity) ShortName() string { return ShortName(id.PublicKey) }

func parts(publicKey []byte) (adjective, noun, suffix string, ok bool) {
	if len(publicKey) != SeedBytes {
		return "", "", "", false
	}

	digest := blake3.Sum256(publicKey)
	return adjectives[digest[0]], nouns[digest[1]], hex.EncodeToString(digest[2:6]), true
}

//go:generate sh -c "cd ../.. && python3 scripts/generate-petname-words.py"
