// Command releases-provider serves the release channels of ARC and the
// archives that they name. It only reads.
//
// A request names one of two operations:
//
//	{"op":"channel","channel":"stable"}
//	{"op":"chunk","digest":"sha256:<hex>","offset":0,"length":65536}
//
// The channel document comes back whole. A chunk comes back as base64, so a
// large archive travels in pieces that each fit one reply.
package main

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

	"github.com/gezibash/arc/provider"
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

func main() {
	root := os.Getenv("RELEASES_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		root = filepath.Join(home, ".local", "share", "arc", "releases")
	}

	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		fmt.Fprintln(os.Stderr, "the release root is not a directory")
		os.Exit(1)
	}

	handler := &server{root: root}
	if err := provider.Run(context.Background(), handler, provider.Options{MaxLineBytes: MaxLineBytes}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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

	decoder := json.NewDecoder(strings.NewReader(request.Message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() {
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
	hex, found := strings.CutPrefix(digest, "sha256:")
	if !found || !digestPattern.MatchString(hex) {
		return "", errInvalidRequest
	}
	if offset < 0 || length <= 0 || length > MaxChunkBytes {
		return "", errInvalidRequest
	}

	if err := s.directory(s.root); err != nil {
		return "", err
	}
	blobs := filepath.Join(s.root, "blobs")
	if err := s.directory(blobs); err != nil {
		return "", err
	}

	path := filepath.Join(blobs, hex+".tar.gz")
	info, err := regularFile(path)
	if err != nil {
		return "", err
	}
	if offset > info.Size() {
		return "", errInvalidRequest
	}

	file, err := os.Open(path)
	if err != nil {
		return "", errStorage
	}
	defer file.Close()

	size := min(int64(length), info.Size()-offset)
	data := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(file, offset, size), data); err != nil {
		return "", errStorage
	}

	answer, err := json.Marshal(map[string]any{
		"digest": "sha256:" + hex,
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
