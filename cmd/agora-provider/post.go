// Command agora-provider keeps a public board of signed posts.
//
// Every post is signed by its author, and the board is the public key of the
// citizen that serves it. A post never changes, and a reply names the post
// that it answers.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/canonical"
	"github.com/gezibash/arc/provider"
)

// The limits of the board.
const (
	MaxBodyBytes    = 4096
	MaxLineBytes    = 64 * 1024
	DefaultMaxPosts = 10_000
	// FreshnessSeconds is how far the time of a post may stand from the
	// time of the board.
	FreshnessSeconds = 300
	// DefaultLimit and MaxLimit bound one page.
	DefaultLimit = 20
	MaxLimit     = 50
)

// SignatureDomain stands at the front of every signed post.
const SignatureDomain = "arc-agora-post-v1\n"

// The errors that a caller may see.
const (
	errInvalidRequest = provider.Error("invalid_request")
	errInvalidPost    = provider.Error("invalid_post")
	errNotFound       = provider.Error("not_found")
	errConflict       = provider.Error("conflict")
	errQuota          = provider.Error("quota_exceeded")
	errStale          = provider.Error("stale_post")
	errStorage        = provider.Error("storage_failure")
	errCorrupt        = provider.Error("corrupt_storage")
	errLocked         = provider.Error("storage_locked")
)

var (
	keyPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	signaturePattern = regexp.MustCompile(`^[a-f0-9]{128}$`)
	noncePattern     = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// post is one signed post of the board.
type post struct {
	Version   int     `json:"version"`
	Author    string  `json:"author"`
	Board     string  `json:"board"`
	Body      string  `json:"body"`
	Parent    *string `json:"parent"`
	CreatedAt int64   `json:"created_at"`
	Nonce     string  `json:"nonce"`
	Signature string  `json:"signature"`
	ID        string  `json:"id"`
}

// validatePost checks one post against the board. The author must have
// signed it, and the id must hold the hash of the message and the signature.
func validatePost(raw json.RawMessage, board string) (*post, error) {
	if !keyPattern.MatchString(board) {
		return nil, errInvalidPost
	}

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()

	held := &post{}
	if err := decoder.Decode(held); err != nil || decoder.More() {
		return nil, errInvalidPost
	}

	switch {
	case held.Version != 1,
		!keyPattern.MatchString(held.Author),
		held.Board != board,
		len(held.Body) > MaxBodyBytes,
		strings.TrimSpace(held.Body) == "",
		held.Parent != nil && !keyPattern.MatchString(*held.Parent),
		held.CreatedAt < 0,
		!noncePattern.MatchString(held.Nonce),
		!signaturePattern.MatchString(held.Signature),
		!keyPattern.MatchString(held.ID):
		return nil, errInvalidPost
	}

	message, err := signatureMessage(held)
	if err != nil {
		return nil, errInvalidPost
	}

	signature, err := hex.DecodeString(held.Signature)
	if err != nil {
		return nil, errInvalidPost
	}
	if held.ID != sum(append([]byte(message), signature...)) {
		return nil, errInvalidPost
	}

	author, err := hex.DecodeString(held.Author)
	if err != nil {
		return nil, errInvalidPost
	}
	if !identity.Verify(author, []byte(message), signature) {
		return nil, errInvalidPost
	}
	return held, nil
}

// signatureMessage is what the author signs: the domain, then the fields of
// the post as one canonical array.
func signatureMessage(held *post) (string, error) {
	var parent any
	if held.Parent != nil {
		parent = *held.Parent
	}

	encoded, err := canonical.Encode([]any{
		1, held.Author, held.Board, held.Body, parent, held.CreatedAt, held.Nonce,
	})
	if err != nil {
		return "", err
	}
	return SignatureDomain + string(encoded), nil
}

func sum(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
