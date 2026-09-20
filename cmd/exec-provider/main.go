package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/gezibash/arc/provider"
)

// server answers the requests of one operator configuration.
type server struct {
	config *config
	lease  *lease
	log    io.Writer
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	handler := &server{config: cfg, lease: newLease(cfg.Lease, os.Stderr), log: os.Stderr}

	// A JSON string grows about six times when it carries escaped text. The
	// cap on one line therefore stands above the body limit.
	maxLine := cfg.Limits.BodyBytes*6 + 8*1024

	if err := provider.Run(context.Background(), handler, provider.Options{MaxLineBytes: maxLine}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// HandleRequest answers one ARC request.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
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

	var reply map[string]any

	switch action {
	case "start":
		reply, err = s.startJob(request.From, cmd)
	case "status":
		reply, err = s.jobStatus(request.From, job)
	default:
		reply, err = s.run(cmd)
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
func (s *server) run(cmd *command) (map[string]any, error) {
	s.lease.hold()
	defer s.lease.release()

	result, err := runCommand(s.config, cmd)
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
