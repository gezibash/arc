package keys

import (
	"fiatjaf.com/nostr"
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
			pk, ok := value.(nostr.PubKey)
			return pk, ok
		case "nprofile":
			pointer, ok := value.(nostr.ProfilePointer)
			return pointer.PublicKey, ok
		}
	}
	return nostr.PubKey{}, false
}
