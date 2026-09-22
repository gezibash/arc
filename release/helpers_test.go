package release_test

import (
	"encoding/base64"

	"fiatjaf.com/nostr"
)

func base64Std(value []byte) string { return base64.StdEncoding.EncodeToString(value) }

// publisherKey is a Nostr key that signs channels.
type publisherKey struct {
	secret    nostr.SecretKey
	PublicKey []byte
}

func newPublisher() (*publisherKey, error) {
	secret := nostr.Generate()
	public := secret.Public()
	return &publisherKey{secret: secret, PublicKey: public[:]}, nil
}
