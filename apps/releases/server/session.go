package server

import (
	"context"
	"encoding/json"
	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"io"
	"strings"
)

func (s *server) HandleSession(ctx context.Context, req provider.Request, stream *session.Stream) error {
	if stream.Mode() != session.ServerStream {
		return session.ErrUnsupported
	}
	if req.Method() != "RAW" || req.Path() != "/releases" {
		return errInvalidRequest
	}
	var request struct {
		Op     string `json:"op"`
		Digest string `json:"digest"`
	}
	decoder := json.NewDecoder(strings.NewReader(req.Message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return errInvalidRequest
	}
	if err := decoder.Decode(new(any)); err != io.EOF || request.Op != "archive" {
		return errInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, info, err := s.openBlob(request.Digest)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(stream, io.NewSectionReader(file, 0, info.Size()))
	return err
}
