package draft

import (
	"context"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"fiatjaf.com/nostr"
	"golang.org/x/crypto/chacha20poly1305"
)

// A body is the raw bytes of a draft, such as a file, kept beside the wrap
// and not in its content. NIP-44 holds at most 65,535 bytes, and a part
// holds text, so a large file in parts costs base64 and one event for each
// 32 KiB. The device encrypts the body once, and the wrap names the
// encrypted bytes in a body tag: their SHA-256 and their size. The
// signature of the wrap thus covers the body. docs/interface/SPEC.md,
// section 3.9.5, defines the format.

// MaxBody bounds the raw bytes of one body. A body travels in the blob of
// its wrap, and the largest blob is 64 MiB.
const MaxBody = 63 << 20

// bodyInfo derives the body key from the content key of an app.
const bodyInfo = "arc-body-v1"

// BodyOverhead is what the encryption adds to the raw bytes: the nonce and
// the tag of the AEAD.
const BodyOverhead = chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

// BodyRef names the encrypted bytes of a body.
type BodyRef struct {
	// Sum is the SHA-256 of the encrypted bytes, in hex.
	Sum string
	// Size is the count of the encrypted bytes.
	Size int64
}

// Tag is the body tag of a wrap.
func (b BodyRef) Tag() nostr.Tag {
	return nostr.Tag{"body", b.Sum, strconv.FormatInt(b.Size, 10)}
}

// Body reads the body tag of a wrap. It returns false when the wrap names
// no body.
func Body(wrap nostr.Event) (BodyRef, bool) {
	if wrap.Kind != Kind {
		return BodyRef{}, false
	}
	tag := wrap.Tags.Find("body")
	if len(tag) < 3 {
		return BodyRef{}, false
	}
	raw, err := hex.DecodeString(tag[1])
	if err != nil || len(raw) != sha256.Size {
		return BodyRef{}, false
	}
	size, err := strconv.ParseInt(tag[2], 10, 64)
	if err != nil || size < BodyOverhead || size > MaxBody+BodyOverhead {
		return BodyRef{}, false
	}
	return BodyRef{Sum: tag[1], Size: size}, true
}

// Check says whether data are the encrypted bytes that ref names.
func (b BodyRef) Check(data []byte) error {
	if int64(len(data)) != b.Size {
		return fmt.Errorf("draft: the body has %d bytes, and the wrap names %d", len(data), b.Size)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != b.Sum {
		return errors.New("draft: the body does not hash to the sum that the wrap names")
	}
	return nil
}

func bodyAEAD(key [32]byte) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, key[:], nil, bodyInfo, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(k)
}

// SealBody encrypts the raw bytes of a body with the body key of the app:
// nonce || XChaCha20-Poly1305(body key, nonce, raw). The caller keeps the
// encrypted bytes, and passes the reference to WrapBody.
func (s Sealer) SealBody(raw []byte) ([]byte, BodyRef, error) {
	if len(raw) > MaxBody {
		return nil, BodyRef{}, fmt.Errorf("draft: a body holds at most %d bytes, not %d", MaxBody, len(raw))
	}
	aead, err := bodyAEAD(s.Key)
	if err != nil {
		return nil, BodyRef{}, err
	}
	out := make([]byte, chacha20poly1305.NonceSizeX, len(raw)+BodyOverhead)
	if _, err := rand.Read(out); err != nil {
		return nil, BodyRef{}, err
	}
	out = aead.Seal(out, out, raw, nil)
	sum := sha256.Sum256(out)
	return out, BodyRef{Sum: hex.EncodeToString(sum[:]), Size: int64(len(out))}, nil
}

// WrapBody seals an event into a draft wrap that names a body.
func (s Sealer) WrapBody(ctx context.Context, d string, inner nostr.Event, body BodyRef, at nostr.Timestamp) (nostr.Event, error) {
	if d == "" {
		return nostr.Event{}, errors.New("draft: a wrap needs a d tag")
	}
	content, err := s.seal(&inner)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, s.Signer, nostr.Event{
		Kind: Kind, CreatedAt: at, Content: content,
		Tags: s.tags(nostr.Tag{"d", d}, nostr.Tag{"k", strconv.Itoa(int(inner.Kind))}, body.Tag()),
	})
}

// OpenBody checks the encrypted bytes of the body of a wrap against its
// body tag, and decrypts them with the body key of the generation of the
// wrap.
func (o Opener) OpenBody(ctx context.Context, wrap nostr.Event, data []byte) ([]byte, error) {
	ref, ok := Body(wrap)
	if !ok {
		return nil, errors.New("draft: the wrap names no body")
	}
	gen, ok := Gen(wrap)
	if !ok {
		return nil, errors.New("draft: a wrap with a body names its generation")
	}
	if err := ref.Check(data); err != nil {
		return nil, err
	}
	key, err := o.Key(ctx, gen)
	if err != nil {
		return nil, fmt.Errorf("draft: %w", err)
	}
	aead, err := bodyAEAD(key)
	if err != nil {
		return nil, err
	}
	raw, err := aead.Open(nil, data[:chacha20poly1305.NonceSizeX], data[chacha20poly1305.NonceSizeX:], nil)
	if err != nil {
		return nil, fmt.Errorf("draft: the body does not open with generation %d", gen)
	}
	return raw, nil
}
