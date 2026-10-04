// Package nip05 resolves internet names to public ARC identities.
package nip05

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip05"
	"github.com/gezibash/arc/core/keys"
)

// ResolvePublic reads a public identity, including a NIP-05 address. Local
// aliases belong to the application's install catalog, not the key codec.
func ResolvePublic(ctx context.Context, text string) (nostr.PubKey, error) {
	if pk, ok := keys.ParsePublic(text); ok {
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
