package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/providertest"
)

// chunks answers the chunk requests of one archive, and counts them.
type chunks struct {
	archive []byte
	digest  string
	asked   int
}

func (c *chunks) Request(_ context.Context, body []byte) ([]byte, error) {
	c.asked++
	var req struct {
		Offset int `json:"offset"`
		Length int `json:"length"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	end := min(req.Offset+req.Length, len(c.archive))
	return json.Marshal(map[string]any{
		"digest": "sha256:" + c.digest, "offset": req.Offset,
		"data": base64.StdEncoding.EncodeToString(c.archive[req.Offset:end]),
	})
}

// archiveOf is an archive and its artifact, with the chunks that serve it.
func archiveOf(text string) (*chunks, *Artifact) {
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	return &chunks{archive: []byte(text), digest: digest}, &Artifact{Size: int64(len(text)), SHA256: digest}
}

// refusing opens the archive session, and the provider ends it with the
// refusal reason, before any record.
func refusing(t *testing.T, reason string) func(context.Context, string) (*session.Stream, error) {
	return func(_ context.Context, body string) (*session.Stream, error) {
		if !strings.Contains(body, `"direct"`) {
			t.Errorf("the archive session was asked for with no offer of a pipe: %s", body)
		}
		return providertest.Start(t, session.ServerStream, func(context.Context, *session.Stream) error {
			return provider.Error(reason)
		}), nil
	}
}

var loopback = direct.Options{Loopback: true}

// TestAnArchivePipeThatDoesNotOpenIsAnError gets an archive from a
// provider whose pipe finds no path. In auto, that is an error: the
// update never asks for the archive in chunks, so it never takes a slower
// path that the user did not choose (docs/updates/SPEC.md, section 3.7).
func TestAnArchivePipeThatDoesNotOpenIsAnError(t *testing.T) {
	source, artifact := archiveOf("the archive")
	p := &pipeCaller{Requester: source, open: refusing(t, "no_path"), dial: direct.DialConfig{Policy: direct.Auto, Options: loopback}}
	if _, err := Download(t.Context(), p, artifact, nil); err == nil || !strings.Contains(err.Error(), "no direct path, and no path through the relay") {
		t.Fatalf("got %v, want the error of no path", err)
	}
	if source.asked != 0 {
		t.Errorf("the update asked for %d chunks after a pipe that did not open", source.asked)
	}
}

// TestAnOlderReleasesProviderSendsChunks gets an archive from a provider
// that is older than the pipe: it refuses the field "direct" as
// invalid_request. In auto, the update says so and reads the archive in
// chunks. With the policy direct, it stops, and names --path session.
func TestAnOlderReleasesProviderSendsChunks(t *testing.T) {
	source, artifact := archiveOf("the archive of an older provider")
	var notes bytes.Buffer
	p := &pipeCaller{Requester: source, open: refusing(t, "invalid_request"), dial: direct.DialConfig{Policy: direct.Auto, Options: loopback, Notes: &notes}}
	got, err := Download(t.Context(), p, artifact, nil)
	if err != nil || string(got) != string(source.archive) {
		t.Fatalf("got %q, %v; want the archive in chunks", got, err)
	}
	if source.asked == 0 || p.path != chunkPath {
		t.Errorf("the archive took the path %q with %d chunks; want chunks", p.path, source.asked)
	}
	if !strings.Contains(notes.String(), "the provider has no pipe; using chunks over the relay") {
		t.Errorf("notes: %q", notes.String())
	}

	source, artifact = archiveOf("the archive of an older provider")
	p = &pipeCaller{Requester: source, open: refusing(t, "invalid_request"), dial: direct.DialConfig{Policy: direct.DirectOnly, Options: loopback}}
	if _, err := Download(t.Context(), p, artifact, nil); err == nil || !strings.Contains(err.Error(), "run again with --path session") {
		t.Fatalf("with the policy direct, got %v; want it to stop and name --path session", err)
	}
	if source.asked != 0 {
		t.Errorf("with the policy direct, the update asked for %d chunks", source.asked)
	}
}

// TestARefusalThatEndsTheOpenIsMapped gets an archive from an older
// provider whose refusal ends the open, before the transport confirms it:
// the open itself gives the refusal as its error. In auto, the update reads
// the archive in chunks, as when the refusal ends the session after it
// opens (sdk/httpadapter TestRefusalBeforeOpenIsConfirmed).
func TestARefusalThatEndsTheOpenIsMapped(t *testing.T) {
	source, artifact := archiveOf("the archive of an older provider")
	open := func(context.Context, string) (*session.Stream, error) {
		return nil, session.RemoteError("invalid_request")
	}
	p := &pipeCaller{Requester: source, open: open, dial: direct.DialConfig{Policy: direct.Auto, Options: loopback}}
	got, err := Download(t.Context(), p, artifact, nil)
	if err != nil || string(got) != string(source.archive) {
		t.Fatalf("got %q, %v; want the archive in chunks", got, err)
	}
	if p.path != chunkPath {
		t.Errorf("the archive took the path %q, want chunks", p.path)
	}
}
