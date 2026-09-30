// Package server serves the release channels of ARC and the
// archives that they name. It only reads.
//
// A request names one of two operations:
//
//	{"op":"channel","channel":"stable"}
//	{"op":"chunk","digest":"sha256:<hex>","offset":0,"length":65536}
//
// The channel document comes back whole. A chunk comes back as base64, so a
// large archive travels in pieces that each fit one reply.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/strictjson"
)

// The limits of the provider.
const (
	// MaxChunkBytes is the largest piece of an archive in one reply.
	MaxChunkBytes = 256 * 1024
	// MaxLineBytes caps one line from ARC.
	MaxLineBytes = 512 * 1024
)

// The errors that a caller may see.
const (
	errInvalidRequest = provider.Error("invalid_request")
	errNotFound       = provider.Error("not_found")
	errStorage        = provider.Error("storage_failure")
	errTooLarge       = provider.Error("response_too_large")
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type server struct {
	root string
}

// Run reads the release root and serves the app through core ARC.
// The caller supplies protocol streams and diagnostics.
func Run(ctx context.Context, opts provider.Options) error {
	root := os.Getenv("RELEASES_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root = filepath.Join(home, ".local", "share", "arc", "releases")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("the release root is not a directory")
	}
	opts.MaxLineBytes = MaxLineBytes
	return provider.Run(ctx, &server{root: root}, opts)
}

// HandleRequest answers one request.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	var body struct {
		Op      string `json:"op"`
		Channel string `json:"channel"`
		Digest  string `json:"digest"`
		Offset  int64  `json:"offset"`
		Length  int    `json:"length"`
	}

	if err := strictjson.Decode(strings.NewReader(request.Message), &body); err != nil {
		return "", errInvalidRequest
	}

	switch body.Op {
	case "channel":
		return s.channel(body.Channel)
	case "chunk":
		return s.chunk(body.Digest, body.Offset, body.Length)
	default:
		return "", errInvalidRequest
	}
}

// channel returns the document of one channel, whole.
func (s *server) channel(name string) (string, error) {
	if name != "stable" && name != "beta" {
		return "", errInvalidRequest
	}
	if err := s.directory(s.root); err != nil {
		return "", err
	}
	if err := s.directory(filepath.Join(s.root, "channels")); err != nil {
		return "", err
	}

	path := filepath.Join(s.root, "channels", name+".json")
	info, err := regularFile(path)
	if err != nil {
		return "", err
	}
	if info.Size() > MaxChunkBytes {
		return "", errTooLarge
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", errStorage
	}
	return string(data), nil
}

// chunk returns one piece of one archive, as base64.
func (s *server) chunk(digest string, offset int64, length int) (string, error) {
	if offset < 0 || length <= 0 || length > MaxChunkBytes {
		return "", errInvalidRequest
	}
	file, info, err := s.openBlob(digest)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if offset > info.Size() {
		return "", errInvalidRequest
	}

	size := min(int64(length), info.Size()-offset)
	data := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(file, offset, size), data); err != nil {
		return "", errStorage
	}

	answer, err := json.Marshal(map[string]any{
		"digest": digest,
		"offset": offset,
		"data":   base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return "", errStorage
	}
	return string(answer), nil
}

// directory refuses a path that is not a directory, and never follows a link.
func (s *server) directory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return errStorage
	}
	return nil
}

// regularFile refuses a link and anything that is not a file.
func regularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errNotFound
	}
	if !info.Mode().IsRegular() {
		return nil, errStorage
	}
	return info, nil
}

// openBlob is shared by chunk requests and live archive streams.
func (s *server) openBlob(digest string) (*os.File, os.FileInfo, error) {
	hex, found := strings.CutPrefix(digest, "sha256:")
	if !found || !digestPattern.MatchString(hex) {
		return nil, nil, errInvalidRequest
	}
	if err := s.directory(s.root); err != nil {
		return nil, nil, err
	}
	blobs := filepath.Join(s.root, "blobs")
	if err := s.directory(blobs); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(blobs, hex+".tar.gz")
	info, err := regularFile(path)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errStorage
	}
	actual, err := file.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		file.Close()
		return nil, nil, errStorage
	}
	return file, actual, nil
}
