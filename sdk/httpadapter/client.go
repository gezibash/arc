package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/sdk/direct"
)

// ErrProviderHasNoPipe says that the provider refused the offer of a pipe:
// it is older than the pipe, or its policy opens no pipe.
var ErrProviderHasNoPipe = errors.New("the provider has no pipe")

// Opener opens one session with an initial request.
type Opener func(ctx context.Context, request string) (*session.Stream, error)

// ClientSession is an HTTP session that a consumer opened.
type ClientSession struct {
	Stream *session.Stream
	// Pipe carries the response body, or it is nil, and session frames
	// carry the body.
	Pipe *direct.Pipe
	// Path names the path of the body, for direct.Report.
	Path   string
	reader *SessionReader
}

// Response is what Copy read.
type Response struct {
	Status   int
	Headers  http.Header
	Trailers http.Header
	// Bytes is the number of body bytes, and Elapsed the time from the
	// "response" record to the last byte.
	Bytes   int64
	Elapsed time.Duration
}

// OpenSession opens an HTTP session for request, the initial request of
// docs/http/SPEC.md, section 3.6. If the policy of c is not direct.Off, the
// request offers a pipe for the response body, and each attempt of the
// policy is one session. In the policy direct.Auto, a provider that has no
// pipe gets the request again with no offer, and notes gets a line. In the
// other policies, that provider gives ErrProviderHasNoPipe. A pipe that
// does not open is an error, never a fall back to session frames.
func OpenSession(ctx context.Context, c direct.DialConfig, request string, open Opener) (*ClientSession, error) {
	notes := c.Notes
	if notes == nil {
		notes = io.Discard
	}
	in, err := decodeHTTPRequest(request)
	if c.Policy == direct.Off || err != nil || in.WebSocket || in.StreamBody {
		return openFrames(ctx, request, open)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(request), &fields) != nil || fields == nil {
		fields = map[string]json.RawMessage{}
	}
	var current *ClientSession
	pipe, err := direct.Dial(ctx, c, func(ctx context.Context, offer direct.Offer) (string, error) {
		if current != nil {
			_ = current.Stream.Close()
			current = nil
		}
		fields["direct"], _ = json.Marshal(offer)
		body, err := json.Marshal(fields)
		if err != nil {
			return "", err
		}
		stream, err := open(ctx, string(body))
		if err != nil {
			return "", err
		}
		current = &ClientSession{Stream: stream, reader: NewSessionReader(stream)}
		record, err := current.reader.Next()
		if err == nil && record.Type != "direct" {
			return "", fmt.Errorf("the provider sent a %q record before the answer of the pipe", record.Type)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = stream.Wait()
			}
			return "", pipeRefusal(err)
		}
		return record.SDP, nil
	})
	if err != nil {
		if current != nil {
			_ = current.Stream.Close()
		}
		if errors.Is(err, ErrProviderHasNoPipe) && c.Policy == direct.Auto {
			fmt.Fprintln(notes, "the provider has no pipe; using session frames over the relay")
			return openFrames(ctx, request, open)
		}
		if errors.Is(err, ErrProviderHasNoPipe) {
			return nil, fmt.Errorf("%w; run again with --path session", err)
		}
		return nil, err
	}
	current.Pipe, current.Path = pipe, pipe.Path()
	return current, nil
}

func openFrames(ctx context.Context, request string, open Opener) (*ClientSession, error) {
	stream, err := open(ctx, request)
	if err != nil {
		return nil, err
	}
	return &ClientSession{Stream: stream, Path: direct.SessionPath, reader: NewSessionReader(stream)}, nil
}

// pipeRefusal maps the final outcome of a session that gave no answer.
func pipeRefusal(err error) error {
	var remote session.RemoteError
	if !errors.As(err, &remote) {
		if err == nil {
			return errors.New("the provider closed the session before the answer of the pipe")
		}
		return err
	}
	switch remote {
	case "invalid_request", session.RemoteError(ErrNoPipe):
		// An older provider refuses the field "direct" as invalid_request.
		return ErrProviderHasNoPipe
	case session.RemoteError(ErrNoPath):
		return direct.ErrNoPath
	case session.RemoteError(ErrPipesBusy):
		return errors.New("the provider has too many pipes open now; try again later")
	}
	return err
}

// Close closes the session and the pipe.
func (c *ClientSession) Close() error {
	if c.Pipe != nil {
		_ = c.Pipe.Close()
	}
	return c.Stream.Close()
}

// Copy writes the response body to w. The body comes from the pipe, or
// from the "body" records. With a pipe, Copy compares the number of bytes
// with body_bytes of the "end" record. It then waits for the final outcome
// of the session.
func (c *ClientSession) Copy(w io.Writer) (Response, error) {
	var out Response
	var started time.Time
	ended := false
	var announced *int64
	type copied struct {
		n   int64
		err error
	}
	var piped chan copied
	for {
		record, err := c.reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A broken pipe closes the session. Its error says more.
			select {
			case r := <-piped:
				if r.err != nil {
					return out, r.err
				}
			default:
			}
			return out, err
		}
		if ended {
			return out, errors.New("HTTP data after end")
		}
		switch record.Type {
		case "response":
			if out.Status != 0 || record.Status < 200 || record.Status > 599 {
				return out, errors.New("invalid HTTP response status")
			}
			out.Status, out.Headers, started = record.Status, record.Headers, time.Now()
			if c.Pipe != nil {
				piped = make(chan copied, 1)
				go func() {
					n, err := io.Copy(w, c.Pipe)
					if errors.Is(err, direct.ErrPipeClosed) {
						err = fmt.Errorf("the pipe closed after %d bytes; the outcome is not known", n)
					}
					piped <- copied{n, err}
					if err != nil {
						// The session cannot end well without the body.
						_ = c.Stream.Close()
					}
				}()
			}
		case "body":
			if out.Status == 0 {
				return out, errors.New("HTTP body before headers")
			}
			if c.Pipe != nil {
				return out, errors.New("a body record in a session with a pipe")
			}
			if _, err = w.Write(record.Data); err != nil {
				return out, err
			}
			out.Bytes += int64(len(record.Data))
		case "trailers":
			out.Trailers = record.Headers
		case "end":
			ended, announced = true, record.BodyBytes
		default:
			return out, fmt.Errorf("unknown HTTP record %q", record.Type)
		}
	}
	// The provider ends the session after the reader has the last byte of
	// the pipe. A session that failed closes the pipe, so the copy stops.
	final := c.Stream.Wait()
	if piped != nil {
		var r copied
		select {
		case r = <-piped:
		default:
			if final != nil {
				_ = c.Pipe.Close()
			}
			r = <-piped
			if final != nil {
				r.err = nil
			}
		}
		if r.err != nil {
			return out, r.err
		}
		out.Bytes = r.n
	}
	out.Elapsed = time.Since(started)
	if final != nil {
		return out, final
	}
	if out.Status == 0 || !ended {
		return out, errors.New("incomplete HTTP response")
	}
	if c.Pipe != nil && (announced == nil || *announced != out.Bytes) {
		want := "no count"
		if announced != nil {
			want = fmt.Sprint(*announced)
		}
		return out, fmt.Errorf("the body has %d bytes, the provider sent %s", out.Bytes, want)
	}
	return out, nil
}
