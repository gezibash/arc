package citizen

import (
	"context"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
)

// Holder answers what needs the citizen secret beyond signing and NIP-44:
// the requests of docs/devices/SPEC.md, section 3.7. A machine that holds
// the key answers them with its keys.Key. A device has no Holder: it reads
// the keyed root from its draft, and calls storage citizens with its
// device key.
type Holder interface {
	keys.Signer
	KeyedRoot(ctx context.Context) ([]byte, error)
	AccountKey(ctx context.Context, storage nostr.PubKey) (keys.Key, error)
	SignJoin(ctx context.Context, account, storage nostr.PubKey) (string, error)
	SignRelease(ctx context.Context, schema int64, encoded []byte) (string, error)
	SignChange(ctx context.Context, blobID string, record nostr.ID) (string, error)
}

// holder is the Holder of this machine. A device has none.
func (sess *Session) holder() (Holder, error) {
	if sess.Holder == nil {
		return nil, errSignerOnly
	}
	return sess.Holder, nil
}
