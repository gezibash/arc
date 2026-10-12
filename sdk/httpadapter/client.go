package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
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
	Path string
	// Body is the session input, or nil. Copy sends it, and then closes
	// the write side. The request body of a stream_body request goes on
	// the pipe, if there is one; other input goes in the session. For a
	// stream_body request, the caller must set Body and must not write to
	// Stream: with a pipe, the provider reads the body only from the pipe.
	Body io.Reader
	// StreamBody says that the request sets stream_body, so Body is the
	// request body.
	StreamBody bool
	reader     *SessionReader
}

// piped says that Body goes on the pipe.
func (c *ClientSession) piped() bool { return c.StreamBody && c.Pipe != nil }

// Response is what Copy read.
type Response struct {
	Status   int
	Headers  http.Header
	Trailers http.Header
	// Bytes is the number of body bytes, and Elapsed the time from the
	// "response" record to the last byte.
	Bytes   int64
	Elapsed time.Duration
	// RequestBytes is the number of request body bytes that the provider
	// read, and RequestSent the number that Copy sent. They differ only if
	// the handler answered before it read the whole body. RequestElapsed
	// is the time from the start of the send to the "end" record.
	RequestBytes   int64
	RequestSent    int64
	RequestElapsed time.Duration
}

// OpenSession opens an HTTP session for request, the initial request of
// docs/http/SPEC.md, section 3.6. If the policy of c is not direct.Off, the
// request offers a pipe for the response body, and for the request body of
// a stream_body request. Each attempt of the policy is one session. In the
// policy direct.Auto, a provider that has no pipe gets the request again
// with no offer, and notes gets a line. In the
// other policies, that provider gives ErrProviderHasNoPipe. A pipe that
// does not open is an error, never a fall back to session frames.
func OpenSession(ctx context.Context, c direct.DialConfig, request string, open Opener) (*ClientSession, error) {
	notes := c.Notes
	if notes == nil {
		notes = io.Discard
	}
	in, err := decodeHTTPRequest(request)
	if c.Policy == direct.Off || err != nil || in.WebSocket {
		return openFrames(ctx, request, open, in.StreamBody)
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
			fmt.Fprintf(notes, "%v; using session frames over the relay\n", err)
			return openFrames(ctx, request, open, in.StreamBody)
		}
		if errors.Is(err, ErrProviderHasNoPipe) {
			return nil, fmt.Errorf("%w; run again with --path session", err)
		}
		return nil, err
	}
	current.Pipe, current.Path, current.StreamBody = pipe, pipe.Path(), in.StreamBody
	return current, nil
}

func openFrames(ctx context.Context, request string, open Opener, streamBody bool) (*ClientSession, error) {
	stream, err := open(ctx, request)
	if err != nil {
		return nil, err
	}
	return &ClientSession{Stream: stream, Path: direct.SessionPath, StreamBody: streamBody, reader: NewSessionReader(stream)}, nil
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
	case "request_too_large":
		// The offer adds about 1 to 3 KiB to the request.
		return fmt.Errorf("%w for this request: with the offer, the request is over the limit of the provider", ErrProviderHasNoPipe)
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
// with body_bytes of the "end" record. If Body is not nil, Copy sends it at
// the same time, and then closes the write side. It then waits for the
// final outcome of the session.
func (c *ClientSession) Copy(w io.Writer) (Response, error) {
	var out Response
	var started, endedAt time.Time
	ended := false
	var announced, requested *int64
	type copied struct {
		n   int64
		err error
	}
	var piped chan copied
	var up *upload
	if c.Body != nil {
		up = c.send()
	}
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
			if up != nil {
				if upErr := c.endUpload(up, err, false, nil, time.Time{}, &out); upErr != nil {
					return out, upErr
				}
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
			ended, announced, requested, endedAt = true, record.BodyBytes, record.RequestBytes, time.Now()
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
	if up != nil {
		if err := c.endUpload(up, final, ended, requested, endedAt, &out); err != nil {
			return out, err
		}
	}
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

// upload is the request body that Copy sends.
type upload struct {
	start time.Time
	sent  atomic.Int64
	// done gets the result. acked says that the provider read the "end"
	// of the pipe, so it has each byte.
	done  chan error
	acked atomic.Bool
}

type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n.Add(int64(n))
	return n, err
}

// send sends Body on the pipe, or in the session input, and then closes
// the write side.
func (c *ClientSession) send() *upload {
	up := &upload{start: time.Now(), done: make(chan error, 1)}
	go func() {
		var err error
		if c.piped() {
			_, err = io.Copy(countWriter{c.Pipe, &up.sent}, c.Body)
			if err == nil {
				// Copy closes the pipe if the session ends first.
				err = c.Pipe.CloseWrite(context.Background())
				up.acked.Store(err == nil)
			}
			if err != nil && !errors.Is(err, direct.ErrPipeClosed) {
				// The body cannot end: stop the session.
				_ = c.Stream.Close()
			}
		} else {
			_, err = io.Copy(countWriter{c.Stream, &up.sent}, c.Body)
			if err == nil {
				err = c.Stream.CloseWrite()
			}
			if err != nil {
				_ = c.Stream.Close()
			}
		}
		up.done <- err
	}()
	return up
}

// endUpload sets the request fields of out after the session ended. With a
// pipe, the "end" record decides: a pipe that closed before it is an error
// with an outcome that is not known; a pipe that the provider closed after
// it is a handler that answered before it read the whole body.
func (c *ClientSession) endUpload(up *upload, final error, ended bool, requested *int64, endedAt time.Time, out *Response) error {
	if !c.piped() {
		// The session ended, so a send that still waits fails soon.
		out.RequestSent = up.sent.Load()
		out.RequestBytes, out.RequestElapsed = out.RequestSent, endedAt.Sub(up.start)
		return nil
	}
	// A send that still runs stops at its next write. Copy does not wait
	// for it: it can wait for input that never comes.
	var err error
	select {
	case err = <-up.done:
	default:
		_ = c.Pipe.Close()
	}
	sent := up.sent.Load()
	out.RequestSent = sent
	if final != nil || !ended {
		var remote session.RemoteError
		if errors.As(final, &remote) && remote == session.RemoteError(ErrPipeFailed) && !up.acked.Load() {
			return fmt.Errorf("the pipe closed after %d bytes of the request body; the outcome is not known", sent)
		}
		return nil
	}
	if err != nil && !errors.Is(err, direct.ErrPipeClosed) {
		return err
	}
	switch {
	case requested == nil:
		return errors.New("the provider sent no count of the request body")
	case *requested > sent || (up.acked.Load() && *requested != sent):
		return fmt.Errorf("the provider read %d bytes of the request body, arc sent %d", *requested, sent)
	}
	out.RequestBytes, out.RequestElapsed = *requested, endedAt.Sub(up.start)
	return nil
}
