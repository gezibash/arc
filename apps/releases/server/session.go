package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/strictjson"
)

// The errors of an archive session with a pipe. See docs/updates/SPEC.md,
// section 3.7. They have the names of the HTTP errors of docs/http/SPEC.md,
// section 3.7.
const (
	// errNoPipe says that the policy of this provider opens no pipe.
	errNoPipe = provider.Error("no_pipe")
	// errPipesBusy says that the provider holds direct.MaxPipes pipes.
	errPipesBusy = provider.Error("busy")
	// errNoPath says that the data channel of the pipe did not open.
	errNoPath = provider.Error("no_path")
	// errPipeFailed says that the pipe closed before the end of the archive.
	errPipeFailed = provider.Error("pipe_failed")
)

// record is one line of an archive session with a pipe.
type record struct {
	Type string `json:"type"`
	// SDP is the answer of the pipe, in a "direct" record.
	SDP string `json:"sdp,omitempty"`
	// BodyBytes is the number of archive bytes on the pipe, in the "end"
	// record.
	BodyBytes *int64 `json:"body_bytes,omitempty"`
}

// HandleSession sends one archive. With no offer, the session carries the
// archive bytes. With an offer, the session carries a "direct" record and an
// "end" record, and the pipe carries the archive bytes.
func (s *server) HandleSession(ctx context.Context, req provider.Request, stream *provider.Stream) error {
	if stream.Mode() != provider.ServerStream {
		return provider.ErrUnsupported
	}
	if req.Method() != "RAW" || req.Path() != "/releases" {
		return errInvalidRequest
	}
	var request struct {
		Op     string        `json:"op"`
		Digest string        `json:"digest"`
		Direct *direct.Offer `json:"direct"`
	}
	if err := strictjson.Decode(strings.NewReader(req.Message), &request); err != nil || request.Op != "archive" {
		return errInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Direct == nil {
		file, info, err := s.openBlob(request.Digest)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		_, err = io.Copy(stream, io.NewSectionReader(file, 0, info.Size()))
		return err
	}

	// Check the digest before the pipe, so that a bad request costs no
	// pipe. Open the archive after the pipe, so that a failed attempt reads
	// nothing.
	if hex, found := strings.CutPrefix(request.Digest, "sha256:"); !found || !digestPattern.MatchString(hex) {
		return errInvalidRequest
	}
	if request.Direct.Check() != nil {
		return errInvalidRequest
	}
	release, err := s.takePipe()
	if err != nil {
		return err
	}
	defer release()
	encoder := json.NewEncoder(stream)
	pipe, answer, err := direct.Accept(ctx, s.pipes, *request.Direct)
	if err != nil {
		return errNoPath
	}
	defer func() { _ = pipe.Close() }()
	if err = encoder.Encode(record{Type: "direct", SDP: answer}); err != nil {
		return err
	}
	if err = pipe.Wait(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errNoPath
	}
	file, info, err := s.openBlob(request.Digest)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	n, err := io.Copy(pipe, io.NewSectionReader(file, 0, info.Size()))
	if errors.Is(err, direct.ErrPipeClosed) {
		return errPipeFailed
	}
	if err != nil {
		return errStorage
	}
	if err = pipe.CloseWrite(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errPipeFailed
	}
	return encoder.Encode(record{Type: "end", BodyBytes: &n})
}

// takePipe takes a slot for one pipe. It refuses an offer when the policy
// opens no pipe, and when the slots are full.
func (s *server) takePipe() (func(), error) {
	if s.pipes.Policy == direct.Off || s.slots == nil {
		return nil, errNoPipe
	}
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	default:
		return nil, errPipesBusy
	}
}
