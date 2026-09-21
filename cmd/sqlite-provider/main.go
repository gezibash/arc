package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/gezibash/arc/provider"
)

// server answers the requests of one operator configuration.
type server struct {
	config *config
}

func main() {
	held, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// A JSON string grows about six times when it carries escaped text, so
	// the cap on one line stands above the body limit.
	maxLine := held.Limits.BodyBytes*6 + 8*1024

	if err := provider.Run(context.Background(), &server{config: held}, provider.Options{MaxLineBytes: maxLine}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// HandleRequest answers one query.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	if request.Method() != "QUERY" || request.Path() == "" {
		return "", errInvalidRequest
	}

	answer, err := s.query(request.From, request.Path(), request.Message)
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(answer)
	if err != nil {
		return "", errQueryFailed
	}
	return string(body), nil
}
