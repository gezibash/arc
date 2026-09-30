package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/gezibash/arc/sdk/provider"
)

// server answers the requests of one operator configuration.
type server struct {
	config *config
	lease  *lease
	log    io.Writer
}

// Run reads the operator configuration and serves the app through core ARC.
// The caller supplies protocol streams and diagnostics.
func Run(ctx context.Context, opts provider.Options) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	handler := &server{config: cfg, lease: newLease(cfg.Lease, opts.Log), log: opts.Log}
	// Escaped JSON can grow to six times the configured request body size.
	opts.MaxLineBytes = cfg.Limits.BodyBytes*6 + 8*1024
	return provider.Run(ctx, handler, opts)
}

// HandleRequest answers one ARC request.
func (s *server) HandleRequest(ctx context.Context, request provider.Request) (string, error) {
	if request.Method() != "EXEC" {
		return "", provider.ErrInvalidRequest
	}
	if !s.config.Grants[request.From] {
		return "", provider.Error("access_denied")
	}

	action, cmd, job, err := parseRequest(s.config, request.Message)
	if err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	var reply map[string]any

	switch action {
	case "start":
		reply, err = s.startJob(request.From, cmd)
	case "status":
		reply, err = s.jobStatus(request.From, job)
	default:
		reply, err = s.run(ctx, cmd)
	}
	if err != nil {
		return "", err
	}

	body, err := encodeJSON(reply)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// run holds the lease while the command runs, so the machine stays awake.
func (s *server) run(ctx context.Context, cmd *command) (map[string]any, error) {
	s.lease.hold()
	defer s.lease.release()

	result, err := runCommand(ctx, s.config, cmd)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"exit":      result.Exit,
		"stdout":    result.Stdout,
		"stderr":    result.Stderr,
		"timed_out": result.TimedOut,
		"truncated": result.Truncated,
	}, nil
}

// encodeJSON writes compact JSON and leaves the text as it is, so the reply
// reads the same in every implementation.
func encodeJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

func readerOf(data []byte) io.Reader {
	return bytes.NewReader(data)
}
