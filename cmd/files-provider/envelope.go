// Command files-provider keeps private files for their owner.
//
// A file arrives sealed and signed. The provider checks the signature of the
// owner and the hash of the body, and stores the envelope as it came. It
// never opens a body, and no other citizen may read one.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/provider"
)

// The limits of one envelope.
const (
	MaxNameBytes = 2048
	MaxBodyBytes = 5_592_500
	MaxLineBytes = 6 * 1024 * 1024
	// DefaultQuota is what one owner may hold. FILES_QUOTA_BYTES changes it.
	DefaultQuota = 64 * 1024 * 1024
)

// The errors that a caller may see.
const (
	errInvalidRequest  = provider.Error("invalid_request")
	errInvalidEnvelope = provider.Error("invalid_envelope")
	errNotFound        = provider.Error("not_found")
	errConflict        = provider.Error("conflict")
	errQuota           = provider.Error("quota_exceeded")
	errStorage         = provider.Error("storage_failure")
	errCorrupt         = provider.Error("corrupt_storage")
)

// SignatureDomain stands at the front of every signed message, so a
// signature of one kind never counts as another.
const SignatureDomain = "arc-private-file-v1\n"

var (
	keyPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	signaturePattern = regexp.MustCompile(`^[a-f0-9]{128}$`)
	tokenPattern     = regexp.MustCompile(`^sealed-v1:[A-Za-z0-9+/=]+$`)
)

// envelope is one private file. The name and the body are sealed to the
// owner, so only the owner reads either one.
type envelope struct {
	Version   int    `json:"version"`
	Owner     string `json:"owner"`
	Name      string `json:"name"`
	BodyHash  string `json:"body_hash"`
	Signature string `json:"signature"`
	ID        string `json:"id"`
	Body      string `json:"body"`
}

// headers is the envelope without its body, which is what a listing shows.
func (e *envelope) headers() map[string]any {
	return map[string]any{
		"version":   e.Version,
		"owner":     e.Owner,
		"name":      e.Name,
		"body_hash": e.BodyHash,
		"signature": e.Signature,
		"id":        e.ID,
	}
}

// validate checks one envelope against its owner. Every field must hold, and
// the owner must have signed the name and the hash of the body.
func validate(raw json.RawMessage, owner string) (*envelope, error) {
	if !keyPattern.MatchString(owner) {
		return nil, errInvalidEnvelope
	}

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()

	held := &envelope{}
	if err := decoder.Decode(held); err != nil || decoder.More() {
		return nil, errInvalidEnvelope
	}

	switch {
	case held.Version != 1,
		held.Owner != owner,
		len(held.Name) > MaxNameBytes,
		len(held.Body) > MaxBodyBytes,
		!sealedToken(held.Name),
		!sealedToken(held.Body),
		!keyPattern.MatchString(held.BodyHash),
		held.BodyHash != sum(held.Body),
		!signaturePattern.MatchString(held.Signature),
		!keyPattern.MatchString(held.ID):
		return nil, errInvalidEnvelope
	}

	message := signatureMessage(owner, held.Name, held.BodyHash)
	if held.ID != sum(message+"\n"+held.Signature) {
		return nil, errInvalidEnvelope
	}

	publicKey, err := hex.DecodeString(owner)
	if err != nil {
		return nil, errInvalidEnvelope
	}
	signature, err := hex.DecodeString(held.Signature)
	if err != nil {
		return nil, errInvalidEnvelope
	}
	if !identity.Verify(publicKey, []byte(message), signature) {
		return nil, errInvalidEnvelope
	}
	return held, nil
}

// signatureMessage is what the owner signs.
func signatureMessage(owner, name, bodyHash string) string {
	return SignatureDomain + owner + "\n" + name + "\n" + bodyHash
}

// sealedToken says whether a value is a sealed box of version 1: the version
// byte, an ephemeral key of 32 bytes, a tag of 16 bytes, and the ciphertext.
func sealedToken(value string) bool {
	if !tokenPattern.MatchString(value) {
		return false
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "sealed-v1:"))
	if err != nil || len(raw) < 1+32+16 || raw[0] != 1 {
		return false
	}
	return true
}

func sum(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
