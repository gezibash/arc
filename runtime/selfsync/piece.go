package selfsync

import (
	"bytes"
	"cmp"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"sync"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/klauspost/reedsolomon"
	"golang.org/x/crypto/chacha20poly1305"
)

// A piece is one shard of a Reed-Solomon code of one blob, sealed alone
// (docs/storage/SPEC.md, section 3.5). Any k pieces of one seal rebuild the
// blob. The code is RS(k, 2k) of github.com/klauspost/reedsolomon: a blob
// has at most 2k pieces, and pieces 0 to k-1 hold the blob itself.

// pieceVersion is the first byte of a piece.
const pieceVersion = 0x02

// headerSize is the size of the header inside a tag: the blob ID (32), the
// seal (8), k (1), the index (1) and log2 of the blob size (1).
const pieceHeaderSize = 32 + 8 + 1 + 1 + 1

// Code returns the code of a new blob for s active storage citizens:
// k = ceil(s / 2) pieces rebuild it, and it has t = min(s, 2k) pieces.
// k = 1 is a whole copy on each storage citizen.
func Code(s int) (k, t int) {
	k = (s + 1) / 2
	return k, min(s, 2*k)
}

// SealID names one coding of one blob. Two seals of one record never mix.
type SealID [8]byte

// Header is what the tag of a piece holds.
type Header struct {
	// Blob is the ID of the blob, in hex.
	Blob  string
	Seal  SealID
	K     int
	Index int
	// Size is the size of the blob, a power of two.
	Size int64
}

// ShardSize is the size of each shard of the blob: ceil(Size / K).
func (h Header) ShardSize() int64 { return (h.Size + int64(h.K) - 1) / int64(h.K) }

// pieceKey derives the key that seals the tags and pieces of a generation.
func pieceKey(dataRoot []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, dataRoot, nil, "arc-storage-piece-v1", 32)
}

// PieceIDKey is the key that names the pieces on one storage citizen:
// HKDF-SHA256 of the keyed root, with the info "arc-storage-piece-id-v1"
// followed by the 32 bytes of the key of the storage citizen. Two storage
// citizens thus see no common piece ID.
func (k Keys) PieceIDKey(storage nostr.PubKey) ([]byte, error) {
	return hkdf.Key(sha256.New, k.keyed, nil, "arc-storage-piece-id-v1"+string(storage[:]), 32)
}

// PieceID names the piece index of a blob on one storage citizen:
// HMAC-SHA256 of its piece ID key, the 32 bytes of the blob ID and the
// index as one byte, in hex.
func PieceID(idKey []byte, blob string, index int) string {
	raw, _ := hex.DecodeString(blob)
	mac := hmac.New(sha256.New, idKey)
	mac.Write(raw)
	mac.Write([]byte{byte(index)})
	return hex.EncodeToString(mac.Sum(nil))
}

// Rank orders the storage citizens for one blob, highest first, by
// HMAC-SHA256 of the placement key, the 32 bytes of the blob ID and the 32
// bytes of the key of the storage citizen. Each blob gets its own order,
// and a storage citizen cannot predict it.
func (k Keys) Rank(blob string, storages []nostr.PubKey) []nostr.PubKey {
	raw, _ := hex.DecodeString(blob)
	score := make(map[nostr.PubKey][]byte, len(storages))
	for _, s := range storages {
		mac := hmac.New(sha256.New, k.Place)
		mac.Write(raw)
		mac.Write(s[:])
		score[s] = mac.Sum(nil)
	}
	out := slices.Clone(storages)
	slices.SortFunc(out, func(a, b nostr.PubKey) int { return -bytes.Compare(score[a], score[b]) })
	return out
}

var (
	codersMu sync.Mutex
	coders   = map[int]reedsolomon.Encoder{}
)

// coder returns the RS(k, 2k) code.
func coder(k int) (reedsolomon.Encoder, error) {
	if k < 1 || k > storage.MaxK {
		return nil, fmt.Errorf("k is %d; it must be from 1 to %d", k, storage.MaxK)
	}
	codersMu.Lock()
	defer codersMu.Unlock()
	if c, ok := coders[k]; ok {
		return c, nil
	}
	c, err := reedsolomon.New(k, k)
	if err != nil {
		return nil, err
	}
	coders[k] = c
	return c, nil
}

// Shards codes a blob into its 2k shards, each of ceil(len(blob) / k)
// bytes. Shards 0 to k-1 hold the blob, with zeros at the end.
func Shards(blob []byte, k int) ([][]byte, error) {
	c, err := coder(k)
	if err != nil {
		return nil, err
	}
	shards, err := c.Split(bytes.Clone(blob))
	if err != nil {
		return nil, err
	}
	return shards, c.Encode(shards)
}

// Rebuild makes the blob of a header from k or more of its shards, by
// index.
func Rebuild(h Header, shards map[int][]byte) ([]byte, error) {
	c, err := coder(h.K)
	if err != nil {
		return nil, err
	}
	all := make([][]byte, 2*h.K)
	for i, s := range shards {
		if i < 0 || i >= len(all) || int64(len(s)) != h.ShardSize() {
			return nil, fmt.Errorf("the shard %d does not fit a code of k = %d", i, h.K)
		}
		all[i] = s
	}
	if err := c.ReconstructData(all); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := c.Join(&out, all, int(h.Size)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// NewSeal makes a random seal.
func NewSeal() (SealID, error) {
	var s SealID
	_, err := rand.Read(s[:])
	return s, err
}

// SealPiece seals the shard of one header: it returns the tag and the
// piece. The tag is the associated data of the piece, so a piece opens
// only with its own tag.
//
//	tag   = gen || nonce || XChaCha20-Poly1305(piece key, nonce, header, aad = gen)
//	piece = 0x02 || gen || nonce || XChaCha20-Poly1305(piece key, nonce, shard, aad = tag)
func SealPiece(k Keys, h Header, shard []byte) (tag, piece []byte, err error) {
	if int64(len(shard)) != h.ShardSize() {
		return nil, nil, errors.New("the shard does not fit the header")
	}
	raw, err := hex.DecodeString(h.Blob)
	if err != nil || len(raw) != 32 {
		return nil, nil, errors.New("the blob ID is not 64 hex digits")
	}
	if h.Size < storage.MinBlob || h.Size&(h.Size-1) != 0 {
		return nil, nil, errors.New("the blob size is not a power of two")
	}
	aead, err := chacha20poly1305.NewX(k.Piece)
	if err != nil {
		return nil, nil, err
	}
	header := make([]byte, 0, pieceHeaderSize)
	header = append(header, raw...)
	header = append(header, h.Seal[:]...)
	header = append(header, byte(h.K), byte(h.Index), byte(bits.TrailingZeros64(uint64(h.Size))))

	tag = make([]byte, 4+chacha20poly1305.NonceSizeX, storage.TagSize)
	binary.BigEndian.PutUint32(tag, k.Gen)
	if _, err := rand.Read(tag[4:]); err != nil {
		return nil, nil, err
	}
	tag = aead.Seal(tag, tag[4:], header, tag[:4])

	piece = make([]byte, 1+4+chacha20poly1305.NonceSizeX, storage.PieceOverhead+len(shard))
	piece[0] = pieceVersion
	binary.BigEndian.PutUint32(piece[1:], k.Gen)
	if _, err := rand.Read(piece[5:]); err != nil {
		return nil, nil, err
	}
	piece = aead.Seal(piece, piece[5:], shard, tag)
	return tag, piece, nil
}

// TagGen reads the generation of a tag, so that the caller can find its
// keys.
func TagGen(tag []byte) (uint32, error) {
	if len(tag) != storage.TagSize {
		return 0, fmt.Errorf("a tag has %d bytes, not %d", storage.TagSize, len(tag))
	}
	return binary.BigEndian.Uint32(tag), nil
}

// OpenTag opens the tag of a piece with the keys of its generation.
func OpenTag(k Keys, tag []byte) (Header, error) {
	var h Header
	gen, err := TagGen(tag)
	if err != nil {
		return h, err
	}
	if gen != k.Gen {
		return h, fmt.Errorf("the tag is of generation %d, not %d", gen, k.Gen)
	}
	aead, err := chacha20poly1305.NewX(k.Piece)
	if err != nil {
		return h, err
	}
	header, err := aead.Open(nil, tag[4:4+chacha20poly1305.NonceSizeX], tag[4+chacha20poly1305.NonceSizeX:], tag[:4])
	if err != nil || len(header) != pieceHeaderSize {
		return h, errors.New("the tag does not open")
	}
	h.Blob = hex.EncodeToString(header[:32])
	copy(h.Seal[:], header[32:40])
	h.K, h.Index = int(header[40]), int(header[41])
	if h.K < 1 || h.K > storage.MaxK || h.Index >= 2*h.K || header[42] < 12 || header[42] > 26 {
		return h, errors.New("the tag names a code that does not exist")
	}
	h.Size = int64(1) << header[42]
	return h, nil
}

// OpenPiece opens a piece with its tag and the header of the tag. It
// returns the shard.
func OpenPiece(k Keys, tag []byte, h Header, piece []byte) ([]byte, error) {
	if int64(len(piece)) != storage.PieceOverhead+h.ShardSize() || piece[0] != pieceVersion {
		return nil, errors.New("the piece has a wrong size or version")
	}
	if !bytes.Equal(piece[1:5], tag[:4]) {
		return nil, errors.New("the piece and its tag name two generations")
	}
	aead, err := chacha20poly1305.NewX(k.Piece)
	if err != nil {
		return nil, err
	}
	shard, err := aead.Open(nil, piece[5:5+chacha20poly1305.NonceSizeX], piece[5+chacha20poly1305.NonceSizeX:], tag)
	if err != nil {
		return nil, errors.New("the piece does not open")
	}
	return shard, nil
}

// compareSeal orders seals, so that each device makes the same choice.
func compareSeal(a, b SealID) int { return cmp.Compare(string(a[:]), string(b[:])) }
