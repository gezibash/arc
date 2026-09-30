package server

import (
	"context"
	"io"
	"strings"

	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/internal/strictjson"
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
	if err := strictjson.Decode(strings.NewReader(req.Message), &request); err != nil || request.Op != "archive" {
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
