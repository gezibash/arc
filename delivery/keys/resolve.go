package keys

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip05"
	"fiatjaf.com/nostr/nip19"
)

// ParsePublic reads the offline forms of a public identity.
func ParsePublic(text string) (nostr.PubKey, bool) {
	if pk, err := nostr.PubKeyFromHex(text); err == nil {
		return pk, true
	}
	if prefix, value, err := nip19.Decode(text); err == nil {
		switch prefix {
		case "npub":
			return value.(nostr.PubKey), true
		case "nprofile":
			return value.(nostr.ProfilePointer).PublicKey, true
		}
	}
	return nostr.PubKey{}, false
}

// ResolvePublic reads a public identity, including a NIP-05 address. Local
// aliases belong to the application's install catalog, not the key codec.
func ResolvePublic(ctx context.Context, text string) (nostr.PubKey, error) {
	if pk, ok := ParsePublic(text); ok {
		return pk, nil
	}
	if strings.Contains(text, ".") && nip05.IsValidIdentifier(text) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		p, err := nip05.QueryIdentifier(ctx, text)
		if err != nil {
			return nostr.PubKey{}, fmt.Errorf("the NIP-05 name %s did not resolve: %w", text, err)
		}
		return p.PublicKey, nil
	}
	return nostr.PubKey{}, fmt.Errorf("%q is not a public key or a NIP-05 name", text)
}
