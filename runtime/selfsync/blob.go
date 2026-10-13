// Package selfsync syncs the self data of a citizen with a storage citizen.
// Self data is what a citizen keeps for itself in its local store: the
// drafts, checkpoints and parts of the data apps, and their deletions. The
// device encrypts each record into one blob, and puts the blob on the
// storage citizen. Another device of the citizen gets the blob, opens it,
// and gives the record to its own store. docs/storage/SPEC.md defines the
// blob and the sync.
package selfsync

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/sdk/storage"
	"golang.org/x/crypto/chacha20poly1305"
)

// Keys are the keys of the blobs of one generation.
type Keys struct {
	// Gen is the generation of the data root that the blob key comes from.
	Gen uint32
	// Blob encrypts the blobs of this generation.
	Blob []byte
	// ID names the blobs. It never changes, so a blob keeps its ID in each
	// generation.
	ID []byte
	// Piece seals the tags and the pieces of this generation.
	Piece []byte
	// Place orders the storage citizens for each blob. It comes from the
	// keyed root, so it never changes.
	Place []byte
	// keyed is the keyed root, for the piece ID key of each storage
	// citizen.
	keyed []byte
}

// NewKeys derives the keys of the blobs of generation gen. The blob key
// comes from the data root of that generation (devices spec, section 3.5).
// The ID key comes from the keyed root of the citizen (interface spec,
// section 3.7.1), so a blob keeps its ID in each generation. The data root
// of generation 0 is the keyed root itself.
func NewKeys(gen uint32, dataRoot, keyedRoot []byte) (Keys, error) {
	if len(dataRoot) != 32 {
		return Keys{}, errors.New("the data root is not 32 bytes")
	}
	if len(keyedRoot) != 32 {
		return Keys{}, errors.New("the keyed root is not 32 bytes")
	}
	blob, err := hkdf.Key(sha256.New, dataRoot, nil, "arc-storage-blob-v1", 32)
	if err != nil {
		return Keys{}, err
	}
	id, err := hkdf.Key(sha256.New, keyedRoot, nil, "arc-storage-id-v1", 32)
	if err != nil {
		return Keys{}, err
	}
	piece, err := pieceKey(dataRoot)
	if err != nil {
		return Keys{}, err
	}
	place, err := hkdf.Key(sha256.New, keyedRoot, nil, "arc-storage-place-v1", 32)
	if err != nil {
		return Keys{}, err
	}
	return Keys{Gen: gen, Blob: blob, ID: id, Piece: piece, Place: place, keyed: bytes.Clone(keyedRoot)}, nil
}

// BlobID names the blob of a record: HMAC-SHA256 of the ID key and the
// record ID, in hex. Two devices that put one record make one blob.
func (k Keys) BlobID(record nostr.ID) string {
	mac := hmac.New(sha256.New, k.ID)
	mac.Write(record[:])
	return hex.EncodeToString(mac.Sum(nil))
}

// The parts of a blob before the ciphertext: the ID, the time, which is
// always 0, and the generation. They are the associated data of the AEAD.
const (
	headerSize   = 32 + 8 + 4
	overheadSize = headerSize + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead
	// lengthSize is the length of the envelope at the start of the padded
	// plaintext.
	lengthSize = 8
)

// changeContext starts the bytes that the device key signs.
const changeContext = "arc-storage-change-v1\x00"

// Envelope is the plaintext of a blob: one record, and the signature of
// the device key that made the change.
//
// Version 1 holds a record with no body. Version 2 holds a draft wrap that
// names a body: the encrypted body follows the JSON in the padded
// plaintext, and Body is its length.
type Envelope struct {
	Version int `json:"v"`
	// Device is the key that signed the change.
	Device nostr.PubKey `json:"device"`
	// Sig is the BIP-340 signature of the device key over changeDigest.
	Sig    string      `json:"sig"`
	Record nostr.Event `json:"record"`
	// Body is the length of the body after the JSON, in version 2.
	Body int64 `json:"body,omitempty"`
}

// changeDigest is what the device key signs: the context, the blob ID and
// the record ID.
func changeDigest(blobID string, record nostr.ID) [32]byte {
	return sha256.Sum256(append(append([]byte(changeContext), blobID...), record[:]...))
}

// blobSize is the size of the blob for an envelope of n bytes: the next
// power of two, at least storage.MinBlob.
func blobSize(n int) int64 {
	need := uint64(overheadSize + lengthSize + n)
	if need <= storage.MinBlob {
		return storage.MinBlob
	}
	return int64(1) << bits.Len64(need-1)
}

// Seal makes the blob of a record. The device key signs the change inside
// the encrypted bytes. A draft wrap that names a body needs that body: the
// encrypted bytes that its body tag names. Each other record has no body.
func Seal(k Keys, device nostr.SecretKey, record nostr.Event, body []byte) (string, []byte, error) {
	env := Envelope{Version: 1, Device: device.Public(), Record: record}
	if ref, ok := draft.Body(record); ok {
		if err := ref.Check(body); err != nil {
			return "", nil, err
		}
		env.Version, env.Body = 2, int64(len(body))
	} else if body != nil {
		return "", nil, errors.New("the record names no body")
	}
	id := k.BlobID(record.ID)
	digest := changeDigest(id, record.ID)
	priv, _ := btcec.PrivKeyFromBytes(device[:])
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return "", nil, err
	}
	env.Sig = hex.EncodeToString(sig.Serialize())
	plain, err := json.Marshal(env)
	if err != nil {
		return "", nil, err
	}
	size := blobSize(len(plain) + len(body))
	if size > storage.MaxBlob {
		return "", nil, fmt.Errorf("the record needs a blob of %d bytes, and the largest blob is %d bytes", size, storage.MaxBlob)
	}
	padded := make([]byte, size-int64(overheadSize))
	binary.BigEndian.PutUint64(padded, uint64(len(plain)))
	copy(padded[lengthSize:], plain)
	copy(padded[lengthSize+len(plain):], body)

	blob := make([]byte, headerSize+chacha20poly1305.NonceSizeX, size)
	raw, _ := hex.DecodeString(id)
	copy(blob, raw)
	binary.BigEndian.PutUint32(blob[40:], k.Gen)
	nonce := blob[headerSize:]
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, err
	}
	aead, err := chacha20poly1305.NewX(k.Blob)
	if err != nil {
		return "", nil, err
	}
	blob = aead.Seal(blob, nonce, padded, blob[:headerSize])
	return id, blob, nil
}

// Gen reads the generation of a blob, so that the caller can find its keys.
func Gen(blob []byte) (uint32, error) {
	if len(blob) < overheadSize {
		return 0, errors.New("the blob is shorter than its header")
	}
	return binary.BigEndian.Uint32(blob[40:]), nil
}

// Open opens the blob with the ID id. It checks the ID in the blob, the
// AEAD, the ID of the record, the signature of the record, the body
// against the body tag of the record, and the signature of the device
// key. It returns the body, or nil for a record with no body. The caller
// checks that the device key may write, and that the citizen signed the
// record.
func Open(k Keys, id string, blob []byte) (Envelope, []byte, error) {
	var env Envelope
	if len(blob) < overheadSize || !storage.GoodSize(int64(len(blob))) {
		return env, nil, errors.New("the blob has a wrong size")
	}
	if hex.EncodeToString(blob[:32]) != id {
		return env, nil, errors.New("the blob holds another ID")
	}
	if gen := binary.BigEndian.Uint32(blob[40:]); gen != k.Gen {
		return env, nil, fmt.Errorf("the blob is of generation %d, not %d", gen, k.Gen)
	}
	aead, err := chacha20poly1305.NewX(k.Blob)
	if err != nil {
		return env, nil, err
	}
	nonce := blob[headerSize : headerSize+chacha20poly1305.NonceSizeX]
	padded, err := aead.Open(nil, nonce, blob[headerSize+chacha20poly1305.NonceSizeX:], blob[:headerSize])
	if err != nil {
		return env, nil, errors.New("the blob does not open")
	}
	n := binary.BigEndian.Uint64(padded)
	room := uint64(len(padded) - lengthSize)
	if n > room {
		return env, nil, errors.New("the length in the blob is larger than the blob")
	}
	if err := json.Unmarshal(padded[lengthSize:lengthSize+n], &env); err != nil || (env.Version != 1 && env.Version != 2) {
		return env, nil, errors.New("the blob holds no envelope of version 1 or 2")
	}
	if !env.Record.CheckID() || k.BlobID(env.Record.ID) != id {
		return env, nil, errors.New("the record in the blob does not match the ID of the blob")
	}
	if !env.Record.VerifySignature() {
		return env, nil, errors.New("the record in the blob has a bad signature")
	}
	var body []byte
	ref, named := draft.Body(env.Record)
	switch {
	case env.Version == 1 && (named || env.Body != 0):
		return env, nil, errors.New("an envelope of version 1 holds no body")
	case env.Version == 2 && !named:
		return env, nil, errors.New("the envelope holds a body, and the record names none")
	case env.Version == 2:
		if env.Body < 0 || uint64(env.Body) > room-n {
			return env, nil, errors.New("the length of the body is larger than the blob")
		}
		body = padded[lengthSize+n : lengthSize+n+uint64(env.Body)]
		if err := ref.Check(body); err != nil {
			return env, nil, err
		}
	}
	raw, err := hex.DecodeString(env.Sig)
	if err != nil {
		return env, nil, errors.New("the change has no signature of a device")
	}
	sig, err := schnorr.ParseSignature(raw)
	if err != nil {
		return env, nil, errors.New("the change has no signature of a device")
	}
	pub, err := schnorr.ParsePubKey(env.Device[:])
	if err != nil {
		return env, nil, errors.New("the device key of the change is not valid")
	}
	digest := changeDigest(id, env.Record.ID)
	if !sig.Verify(digest[:], pub) {
		return env, nil, errors.New("the signature of the device does not match the change")
	}
	return env, body, nil
}
