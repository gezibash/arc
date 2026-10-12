// Command pipe-releases is a releases provider for the tests of the pipe.
// It serves the channel and the chunks of RELEASES_ROOT, as the releases
// app does. PIPE_RELEASES_MODE picks the behavior of an archive session:
//
//	old    a provider that is older than the pipe: it refuses the field
//	       "direct" with invalid_request
//	deaf   it answers each offer, but no pipe opens
//	break  the pipe opens, and closes after 64 KiB of the archive
//	bad    the pipe carries the archive with one byte changed, and the end
//	       record gives the right count
//
// PIPE_RELEASES_LOG gets one line for each archive session, "offer", and
// one line for the first chunk of an archive, "chunks".
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/stdio"
	"github.com/gezibash/arc/sdk/strictjson"
)

type app struct {
	root, mode string
}

type record struct {
	Type      string `json:"type"`
	SDP       string `json:"sdp,omitempty"`
	BodyBytes *int64 `json:"body_bytes,omitempty"`
}

func (a *app) HandleRequest(_ context.Context, r provider.Request) (string, error) {
	var in struct {
		Op      string `json:"op"`
		Channel string `json:"channel"`
		Digest  string `json:"digest"`
		Offset  int64  `json:"offset"`
		Length  int64  `json:"length"`
	}
	if err := json.Unmarshal([]byte(r.Message), &in); err != nil {
		return "", provider.ErrInvalidRequest
	}
	if in.Op == "channel" {
		data, err := os.ReadFile(filepath.Join(a.root, "channels", filepath.Base(in.Channel)+".json"))
		return string(data), err
	}
	if in.Offset == 0 {
		note("chunks")
	}
	data, err := a.archive(in.Digest)
	if err != nil {
		return "", err
	}
	end := min(in.Offset+in.Length, int64(len(data)))
	answer, err := json.Marshal(map[string]any{"digest": in.Digest, "offset": in.Offset,
		"data": base64.StdEncoding.EncodeToString(data[in.Offset:end])})
	return string(answer), err
}

func (a *app) archive(digest string) ([]byte, error) {
	hex, _ := strings.CutPrefix(digest, "sha256:")
	return os.ReadFile(filepath.Join(a.root, "blobs", filepath.Base(hex)+".tar.gz"))
}

func (a *app) HandleSession(ctx context.Context, r provider.Request, stream *provider.Stream) error {
	var in struct {
		Op     string        `json:"op"`
		Digest string        `json:"digest"`
		Direct *direct.Offer `json:"direct"`
	}
	if a.mode == "old" {
		// The request fields of the releases app before the pipe.
		var old struct {
			Op     string `json:"op"`
			Digest string `json:"digest"`
		}
		if strictjson.Decode(strings.NewReader(r.Message), &old) != nil {
			note("offer")
			return provider.ErrInvalidRequest
		}
	}
	if err := json.Unmarshal([]byte(r.Message), &in); err != nil || in.Direct == nil {
		return provider.ErrInvalidRequest
	}
	note("offer")
	data, err := a.archive(in.Digest)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stream)
	if a.mode == "deaf" {
		pc, err := direct.NewConnection(direct.Options{Loopback: true})
		if err != nil {
			return err
		}
		if _, err = pc.CreateDataChannel("pipe", nil); err != nil {
			return err
		}
		offer, err := pc.CreateOffer(nil)
		if err == nil {
			offer, err = direct.Describe(pc, offer)
		}
		_ = pc.Close()
		if err != nil {
			return err
		}
		// An SDP of an end that is gone: no data channel opens.
		answer := strings.Replace(offer.SDP, "a=setup:actpass", "a=setup:active", 1)
		if err = encoder.Encode(record{Type: "direct", SDP: answer}); err != nil {
			return err
		}
		<-ctx.Done()
		return provider.Error("no_path")
	}
	pipe, answer, err := direct.Accept(ctx, direct.AcceptConfig{Policy: direct.Auto, Options: direct.Options{Loopback: true}}, *in.Direct)
	if err != nil {
		return err
	}
	defer func() { _ = pipe.Close() }()
	if err = encoder.Encode(record{Type: "direct", SDP: answer}); err != nil {
		return err
	}
	if err = pipe.Wait(ctx); err != nil {
		return provider.Error("no_path")
	}
	switch a.mode {
	case "break":
		_, _ = pipe.Write(data[:64<<10])
		_ = pipe.Close()
		<-ctx.Done()
		return provider.Error("pipe_failed")
	case "bad":
		data = append([]byte(nil), data...)
		data[len(data)/2] ^= 0xff
	}
	if _, err = pipe.Write(data); err != nil {
		return provider.Error("pipe_failed")
	}
	if err = pipe.CloseWrite(ctx); err != nil {
		return provider.Error("pipe_failed")
	}
	n := int64(len(data))
	return encoder.Encode(record{Type: "end", BodyBytes: &n})
}

// note writes one line to PIPE_RELEASES_LOG.
func note(line string) {
	f, err := os.OpenFile(os.Getenv("PIPE_RELEASES_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func main() {
	stdio.Main("pipe-releases", func(ctx context.Context, opts provider.Options) error {
		return provider.Run(ctx, &app{root: os.Getenv("RELEASES_ROOT"), mode: os.Getenv("PIPE_RELEASES_MODE")}, opts)
	})
}
