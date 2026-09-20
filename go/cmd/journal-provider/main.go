package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/go/provider"
)

// MaxLineBytes caps one line from ARC. An attachment travels as base64, so
// the cap stands above the blob limit.
const MaxLineBytes = 64 * 1024 * 1024

type server struct {
	store  *store
	index  *index
	pusher *pusher
}

func main() {
	root := os.Getenv("JOURNAL_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		root = filepath.Join(home, ".arc", "journal")
	}
	if strings.HasPrefix(root, "~") {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, strings.TrimPrefix(root, "~"))
	}

	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	held := &store{root: root}
	if err := held.ensure(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	handler := &server{
		store:  held,
		index:  newIndex(root),
		pusher: &pusher{store: held, remote: os.Getenv("JOURNAL_REMOTE"), log: os.Stderr},
	}

	// A commit starts the reindex and the push, each after its own pause.
	held.afterCommit = func() {
		handler.index.touch()
		handler.pusher.touch()
	}

	if err := provider.Run(context.Background(), handler, provider.Options{MaxLineBytes: MaxLineBytes}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// HandleRequest answers one command.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	return s.run(request.From, request.Message)
}
