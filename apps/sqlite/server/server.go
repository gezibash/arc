package server

import (
	"context"
	"encoding/json"

	"github.com/gezibash/arc/sdk/provider"
)

// server answers the requests of one operator configuration.
type server struct {
	config *config
}

// Run reads the operator configuration and serves the app through core ARC.
// The caller supplies protocol streams and diagnostics.
func Run(ctx context.Context, opts provider.Options) error {
	held, err := loadConfig()
	if err != nil {
		return err
	}
	// Escaped JSON can grow to six times the configured request body size.
	opts.MaxLineBytes = held.Limits.BodyBytes*6 + 8*1024
	return provider.Run(ctx, &server{config: held}, opts)
}

// HandleRequest answers one query.
func (s *server) HandleRequest(ctx context.Context, request provider.Request) (string, error) {
	if request.Method() != "QUERY" || request.Path() == "" {
		return "", errInvalidRequest
	}

	answer, err := s.query(ctx, request.From, request.Path(), request.Message)
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(answer)
	if err != nil {
		return "", errQueryFailed
	}
	return string(body), nil
}
