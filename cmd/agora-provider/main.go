package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/gezibash/arc/provider"
)

type server struct {
	store *store
	board string
}

func main() {
	// The board is the citizen that serves it.
	board := os.Getenv("ARC_PUBLIC_KEY")
	if !keyPattern.MatchString(board) {
		fmt.Fprintln(os.Stderr, "invalid arc_public_key")
		os.Exit(1)
	}

	root := os.Getenv("AGORA_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		root = filepath.Join(home, ".local", "share", "arc", "agora", board)
	}

	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	maxPosts := DefaultMaxPosts
	if given := os.Getenv("AGORA_MAX_POSTS"); given != "" {
		maxPosts, err = strconv.Atoi(given)
		if err != nil || maxPosts <= 0 {
			fmt.Fprintln(os.Stderr, "invalid AGORA_MAX_POSTS")
			os.Exit(1)
		}
	}

	held := &store{root: root, board: board, maxPosts: maxPosts}
	if err := held.lock(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer held.unlock()

	// A stop signal releases the lock, so the next server starts at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler := &server{store: held, board: board}
	if err := provider.Run(ctx, handler, provider.Options{MaxLineBytes: MaxLineBytes}); err != nil {
		held.unlock()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// HandleRequest answers one request of the board.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	if !keyPattern.MatchString(request.From) || len(request.Message) > MaxLineBytes {
		return "", errInvalidRequest
	}

	var body struct {
		Op    string          `json:"op"`
		Post  json.RawMessage `json:"post"`
		ID    string          `json:"id"`
		After *int            `json:"after"`
		Limit *int            `json:"limit"`
	}

	decoder := json.NewDecoder(strings.NewReader(request.Message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		return "", errInvalidRequest
	}

	after := 0
	if body.After != nil {
		if *body.After <= 0 {
			return "", errInvalidRequest
		}
		after = *body.After
	}

	limit := DefaultLimit
	if body.Limit != nil {
		if *body.Limit < 1 || *body.Limit > MaxLimit {
			return "", errInvalidRequest
		}
		limit = *body.Limit
	}

	var answer map[string]any
	var err error

	switch body.Op {
	case "post":
		if len(body.Post) == 0 {
			return "", errInvalidPost
		}
		held, invalid := validatePost(body.Post, s.board)
		if invalid != nil || held.Author != request.From {
			return "", errInvalidPost
		}
		answer, err = s.store.put(held)

	case "read":
		answer, err = s.store.read(body.ID)

	case "feed":
		answer, err = s.pageAnswer(nil, after, limit, nil)

	case "thread":
		if !keyPattern.MatchString(body.ID) {
			return "", errInvalidRequest
		}
		parent, readErr := s.store.read(body.ID)
		if readErr != nil {
			return "", readErr
		}
		answer, err = s.pageAnswer(&body.ID, after, limit, parent["post"])

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

// pageAnswer builds one page, and names the parent when the caller asked for
// a thread.
func (s *server) pageAnswer(parent *string, after, limit int, held any) (map[string]any, error) {
	posts, next, err := s.store.page(parent, after, limit)
	if err != nil {
		return nil, err
	}

	answer := map[string]any{"posts": posts, "next": next}
	if held != nil {
		answer["post"] = held
	}
	return answer, nil
}
