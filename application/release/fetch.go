package release

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

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

// ChunkBytes is how much of an archive one reply carries. A reply travels
// in a gift wrap, which grows it about 2.4 times, and a relay takes an event
// of at most 256 KiB.
const ChunkBytes = 64 * 1024

// Requester sends one request body to the releases provider, RAW
// /releases, and returns the body of the reply. The client gives one.
type Requester interface {
	Request(ctx context.Context, body []byte) ([]byte, error)
}

// FetchChannel reads one signed channel document from a provider.
func FetchChannel(ctx context.Context, provider Requester, channel string) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"op": "channel", "channel": channel})
	if err != nil {
		return nil, err
	}

	answer, err := provider.Request(ctx, body)
	if err != nil {
		return nil, err
	}

	// The signature covers the digits that arrived, so the numbers keep
	// their form.
	value, err := canonical.Decode(answer)
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
func Download(ctx context.Context, provider Requester, artifact *Artifact, progress func(read, total int64)) ([]byte, error) {
	if artifact == nil || !hexPattern.MatchString(artifact.SHA256) || artifact.Size <= 0 {
		return nil, ErrInvalid
	}

	if streaming, ok := provider.(ArchiveSource); ok {
		return downloadStream(ctx, streaming, artifact, progress)
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

		answer, err := provider.Request(ctx, body)
		if err != nil {
			return nil, err
		}

		var reply struct {
			Digest string `json:"digest"`
			Offset int64  `json:"offset"`
			Data   string `json:"data"`
		}
		if err := json.Unmarshal(answer, &reply); err != nil {
			return nil, fmt.Errorf("release: the provider answered %q", strings.TrimSpace(string(answer)))
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

// Newest reads the channel, checks it against the checkpoint, and records
// it. It returns the newest release for the platform that is later than
// version, or ErrNoRelease.
func Newest(ctx context.Context, provider Requester, checkpoint *Checkpoint, publisher []byte, channel string, platform Platform, version string) (*Release, error) {
	document, err := FetchChannel(ctx, provider, channel)
	if err != nil {
		return nil, err
	}
	// A citizen remembers the newest document that it accepted, so that an
	// older signed document cannot hold it on an old release.
	expect, err := checkpoint.Read(publisher, channel)
	if err != nil {
		return nil, err
	}
	verified, err := Verify(document, expect)
	if err != nil {
		return nil, err
	}
	if err := checkpoint.Write(publisher, verified); err != nil {
		return nil, err
	}
	return verified.Select(platform, version)
}

// Apply downloads the archive of a release, and replaces the program with
// the program of the same name in it.
func Apply(ctx context.Context, provider Requester, newest *Release, program string, progress func(read, total int64)) error {
	if !newest.Eligible {
		return errors.New("release: the channel says that this release cannot be installed")
	}
	archive, err := Download(ctx, provider, newest.Archive(), progress)
	if err != nil {
		return err
	}
	binary, err := Unpack(archive, filepath.Base(program))
	if err != nil {
		return err
	}
	return Replace(ctx, program, binary, newest.Version)
}

// ArchiveReader requires a final protocol outcome in addition to byte EOF.
type ArchiveReader interface {
	io.ReadCloser
	Wait() error
}

// ArchiveSource is selected before submission, when the provider declares streaming.
type ArchiveSource interface {
	OpenArchive(context.Context, string) (ArchiveReader, error)
}

func downloadStream(ctx context.Context, source ArchiveSource, artifact *Artifact, progress func(int64, int64)) ([]byte, error) {
	stream, err := source.OpenArchive(ctx, "sha256:"+artifact.SHA256)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	out := make([]byte, 0, min(artifact.Size, 1024*1024))
	hash := sha256.New()
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := stream.Read(buf)
		if int64(n) > artifact.Size-int64(len(out)) {
			return nil, fmt.Errorf("release: archive exceeds its declared size")
		}
		if n > 0 {
			out = append(out, buf[:n]...)
			hash.Write(buf[:n])
			if progress != nil {
				progress(int64(len(out)), artifact.Size)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if err := stream.Wait(); err != nil {
		return nil, err
	}
	if int64(len(out)) != artifact.Size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return nil, fmt.Errorf("release: archive does not match its size and hash")
	}
	return out, nil
}
