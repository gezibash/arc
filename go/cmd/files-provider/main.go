package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gezibash/arc/go/provider"
)

type server struct {
	store *store
}

func main() {
	root := os.Getenv("FILES_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		root = filepath.Join(home, ".arc", "files")
	}

	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	quota := DefaultQuota
	if given := os.Getenv("FILES_QUOTA_BYTES"); given != "" {
		quota, err = strconv.Atoi(given)
		if err != nil || quota <= 0 {
			fmt.Fprintln(os.Stderr, "invalid FILES_QUOTA_BYTES")
			os.Exit(1)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "objects"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	handler := &server{store: &store{root: root, quota: quota}}
	if err := provider.Run(context.Background(), handler, provider.Options{MaxLineBytes: MaxLineBytes}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// HandleRequest answers one request. Every request reads or writes the files
// of the caller alone.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	if !keyPattern.MatchString(request.From) || len(request.Message) > MaxLineBytes {
		return "", errInvalidRequest
	}

	var body struct {
		Op    string          `json:"op"`
		File  json.RawMessage `json:"file"`
		ID    string          `json:"id"`
		After *string         `json:"after"`
	}

	decoder := json.NewDecoder(strings.NewReader(request.Message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		return "", errInvalidRequest
	}

	var answer map[string]any
	var err error

	switch body.Op {
	case "put":
		if len(body.File) == 0 {
			return "", errInvalidRequest
		}
		held, invalid := validate(body.File, request.From)
		if invalid != nil {
			return "", invalid
		}
		answer, err = s.store.put(request.From, held)

	case "get":
		answer, err = s.store.get(request.From, body.ID)

	case "list":
		cursor := ""
		if body.After != nil {
			cursor = *body.After
		}
		answer, err = s.store.list(request.From, cursor)

	default:
		return "", errInvalidRequest
	}

	if err != nil {
		return "", err
	}

	out, err := json.Marshal(answer)
	if err != nil {
		return "", errStorage
	}
	return string(out), nil
}
