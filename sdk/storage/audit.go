package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// NonceSize is the size of the nonce of an audit, in bytes.
const NonceSize = 32

// auditLabel separates the hash of an audit from each other SHA-256 of
// ARC.
const auditLabel = "arc-storage-audit-v1"

// NewNonce makes the random nonce of one audit. A device makes a new nonce
// for each audit, and never sends a nonce twice.
func NewNonce() []byte {
	nonce := make([]byte, NonceSize)
	_, _ = rand.Read(nonce)
	return nonce
}

// IsNonce says if nonce is the nonce of an audit: NonceSize bytes, in
// lower-case hex.
func IsNonce(nonce string) bool {
	b, err := hex.DecodeString(nonce)
	return err == nil && len(b) == NonceSize && hex.EncodeToString(b) == nonce
}

// AuditHash starts the proof of an audit: the SHA-256 of the label, a
// zero byte, the nonce and the ID. The caller writes the bytes of the blob
// to it. The nonce comes before the bytes, so a storage citizen cannot
// hash the bytes before it knows the nonce.
func AuditHash(nonce []byte, id string) hash.Hash {
	h := sha256.New()
	h.Write([]byte(auditLabel))
	h.Write([]byte{0})
	h.Write(nonce)
	raw, _ := hex.DecodeString(id)
	h.Write(raw)
	return h
}

// AuditProof is the answer to an audit of the blob with the ID, for the
// nonce, in hex.
func AuditProof(nonce []byte, id string, blob []byte) string {
	h := AuditHash(nonce, id)
	h.Write(blob)
	return hex.EncodeToString(h.Sum(nil))
}
