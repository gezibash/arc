package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/release"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/terminal"
)

// An archive goes over a pipe of sdk/direct. The session on the relay keeps
// the request, the "direct" record with the answer, the "end" record and the
// final outcome. See docs/updates/SPEC.md, section 3.7.

// chunkPath is the name of the path of an archive in chunks of live calls.
const chunkPath = "chunks (relay replies)"

// errProviderHasNoPipe says that the releases provider refused the offer of
// a pipe: it is older than the pipe, or its policy opens no pipe.
var errProviderHasNoPipe = errors.New("the provider has no pipe")

// archiveRecord is one record of an archive session with a pipe.
type archiveRecord struct {
	Type      string `json:"type"`
	SDP       string `json:"sdp"`
	BodyBytes *int64 `json:"body_bytes"`
}

// pipeReleaseCaller gets the archive over a pipe. In the policy auto, a
// provider that has no pipe sends the archive in chunks.
type pipeReleaseCaller struct {
	releaseCaller
	installs catalog.Installs
	dial     direct.DialConfig
	// path names the path of the archive, after OpenArchive.
	path string
}

func (r *pipeReleaseCaller) OpenArchive(ctx context.Context, digest string) (release.ArchiveReader, error) {
	var current *pipeArchive
	pipe, err := direct.Dial(ctx, r.dial, func(ctx context.Context, offer direct.Offer) (string, error) {
		if current != nil {
			_ = current.stream.Close()
			current = nil
		}
		body, err := json.Marshal(map[string]any{"op": "archive", "digest": digest, "direct": offer})
		if err != nil {
			return "", err
		}
		stream, err := r.sess.OpenSessionAddress(ctx, r.installs, "releases+arc://"+r.provider.Hex()+"/releases", string(body), session.ServerStream)
		if err != nil {
			return "", err
		}
		current = &pipeArchive{stream: stream, records: json.NewDecoder(stream)}
		var answer archiveRecord
		err = current.records.Decode(&answer)
		if err == nil && answer.Type != "direct" {
			return "", fmt.Errorf("the provider sent a %q record before the answer of the pipe", answer.Type)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = stream.Wait()
			}
			return "", archiveRefusal(err)
		}
		return answer.SDP, nil
	})
	if err != nil {
		if current != nil {
			_ = current.stream.Close()
		}
		if errors.Is(err, errProviderHasNoPipe) && r.dial.Policy == direct.Auto {
			if r.dial.Notes != nil {
				fmt.Fprintf(r.dial.Notes, "%s; using chunks over the relay\n", terminal.EscapeLine(err.Error()))
			}
			r.path = chunkPath
			return nil, release.ErrUseChunks
		}
		if errors.Is(err, errProviderHasNoPipe) {
			return nil, fmt.Errorf("%w; run again with --path session", err)
		}
		return nil, err
	}
	r.path = pipe.Path()
	current.pipe = pipe
	current.done = make(chan struct{})
	go current.follow()
	return current, nil
}

// archiveRefusal maps the final outcome of a session that gave no answer.
func archiveRefusal(err error) error {
	var remote session.RemoteError
	if !errors.As(err, &remote) {
		if err == nil {
			return errors.New("the provider closed the session before the answer of the pipe")
		}
		return err
	}
	switch remote {
	case "invalid_request", "no_pipe":
		// An older provider refuses the field "direct" as invalid_request.
		return errProviderHasNoPipe
	case "request_too_large":
		// An older provider takes a request of at most 4096 bytes. The
		// offer adds about 1 to 3 KiB.
		return fmt.Errorf("%w for this request: with the offer, the request is over the limit of the provider", errProviderHasNoPipe)
	case "no_path":
		return direct.ErrNoPath
	case "busy":
		return errors.New("the provider has too many pipes open now; try again later")
	}
	return err
}

// pipeArchive reads the archive from the pipe. Wait checks the "end"
// record and the final outcome of the session.
type pipeArchive struct {
	stream  *session.Stream
	records *json.Decoder
	pipe    *direct.Pipe
	n       int64

	// follow sets end and err, then closes done.
	done chan struct{}
	end  *int64
	err  error
	once sync.Once
}

// follow reads the "end" record and the final outcome of the session. A
// session that fails closes the pipe, so a blocked Read stops.
func (a *pipeArchive) follow() {
	defer close(a.done)
	var end archiveRecord
	err := a.records.Decode(&end)
	switch {
	case err == nil && (end.Type != "end" || end.BodyBytes == nil):
		err = fmt.Errorf("the provider sent a %q record, not the end", end.Type)
	case err == nil:
		var extra json.RawMessage
		if a.records.Decode(&extra) != io.EOF {
			err = errors.New("the provider sent data after the end")
		}
	case errors.Is(err, io.EOF):
		err = errors.New("the session ended with no end record")
	}
	if final := a.stream.Wait(); final != nil {
		err = final
	}
	a.end, a.err = end.BodyBytes, err
	if err != nil {
		_ = a.pipe.Close()
	}
}

func (a *pipeArchive) Read(b []byte) (int, error) {
	n, err := a.pipe.Read(b)
	a.n += int64(n)
	if errors.Is(err, direct.ErrPipeClosed) {
		// The session can say why the pipe closed.
		select {
		case <-a.done:
			if a.err != nil {
				return n, a.err
			}
		case <-time.After(time.Second):
		}
		_ = a.stream.Close()
		return n, fmt.Errorf("the pipe closed after %d bytes; the outcome is not known", a.n)
	}
	return n, err
}

// Wait waits for the "end" record and the final outcome, and compares the
// number of bytes on the pipe with the end record.
func (a *pipeArchive) Wait() error {
	<-a.done
	if a.err != nil {
		return a.err
	}
	if *a.end != a.n {
		return fmt.Errorf("the archive has %d bytes, the provider sent %d", a.n, *a.end)
	}
	return nil
}

func (a *pipeArchive) Close() error {
	a.once.Do(func() { _ = a.pipe.Close() })
	return a.stream.Close()
}
