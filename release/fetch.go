package release

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/internal/canonical"
)

// A citizen reads a channel and its archives from a releases provider, over
// ARC. The provider answers two operations:
//
//	{"op":"channel","channel":"stable"}
//	{"op":"chunk","digest":"sha256:<hex>","offset":0,"length":65536}
//
// The channel document comes back whole. An archive comes back in pieces,
// and the hash of the whole archive is checked at the end.

// ChunkBytes is how much of an archive one reply carries.
const ChunkBytes = 192 * 1024

// Requester asks another citizen. The client of this module gives one.
type Requester interface {
	Request(ctx context.Context, peer []byte, meta map[string]any, body []byte) (*frame.Frame, error)
}

// FetchChannel reads one signed channel document from a provider.
func FetchChannel(ctx context.Context, peers Requester, provider []byte, channel string) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"op": "channel", "channel": channel})
	if err != nil {
		return nil, err
	}

	answer, err := peers.Request(ctx, provider, map[string]any{"method": "RAW", "path": "/releases"}, body)
	if err != nil {
		return nil, err
	}

	// The signature covers the digits that arrived, so the numbers keep
	// their form.
	value, err := canonical.Decode(answer.Body)
	if err != nil {
		return nil, ErrInvalid
	}

	document, ok := value.(map[string]any)
	if !ok {
		return nil, ErrInvalid
	}
	return document, nil
}

// Download reads one archive from a provider, and checks its size and its
// hash. The progress function, when given, sees how much has arrived.
func Download(ctx context.Context, peers Requester, provider []byte, artifact *Artifact, progress func(read, total int64)) ([]byte, error) {
	if artifact == nil || !hexPattern.MatchString(artifact.SHA256) || artifact.Size <= 0 {
		return nil, ErrInvalid
	}

	out := make([]byte, 0, artifact.Size)

	for int64(len(out)) < artifact.Size {
		length := ChunkBytes
		if left := artifact.Size - int64(len(out)); left < int64(length) {
			length = int(left)
		}

		body, err := json.Marshal(map[string]any{
			"op": "chunk", "digest": "sha256:" + artifact.SHA256,
			"offset": len(out), "length": length,
		})
		if err != nil {
			return nil, err
		}

		answer, err := peers.Request(ctx,
			provider, map[string]any{"method": "RAW", "path": "/releases"}, body)
		if err != nil {
			return nil, err
		}

		var reply struct {
			Digest string `json:"digest"`
			Offset int64  `json:"offset"`
			Data   string `json:"data"`
		}
		if err := json.Unmarshal(answer.Body, &reply); err != nil {
			return nil, fmt.Errorf("release: the provider answered %q", strings.TrimSpace(string(answer.Body)))
		}
		if reply.Digest != "sha256:"+artifact.SHA256 || reply.Offset != int64(len(out)) {
			return nil, ErrInvalid
		}

		piece, err := base64.StdEncoding.DecodeString(reply.Data)
		if err != nil {
			return nil, ErrInvalid
		}
		if len(piece) == 0 {
			return nil, fmt.Errorf("release: the archive ended after %d bytes of %d", len(out), artifact.Size)
		}

		out = append(out, piece...)
		if progress != nil {
			progress(int64(len(out)), artifact.Size)
		}
	}

	digest := sha256.Sum256(out)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return nil, fmt.Errorf("release: the archive does not match its hash")
	}
	return out, nil
}
