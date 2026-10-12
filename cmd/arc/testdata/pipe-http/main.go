// Command pipe-http is an HTTP provider for the tests of the pipe. Each
// session sends 1 MiB. PIPE_HTTP_MODE picks the behavior:
//
//	old    a provider that is older than the pipe: it refuses the field
//	       "direct" with invalid_request, and sends the body in frames
//	deaf   it answers each offer, but no pipe opens
//	break  the pipe opens, and closes after 64 KiB of the body; in an
//	       upload, after 64 KiB of the request body
//
// PIPE_HTTP_LOG gets one line for each session: "offer" or "frames".
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gezibash/arc/sdk/direct"
	httpadapter "github.com/gezibash/arc/sdk/httpadapter"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/stdio"
	"github.com/gezibash/arc/sdk/strictjson"
)

const size = 1 << 20

type app struct {
	frames *httpadapter.Adapter
	mode   string
}

func (a *app) HandleRequest(ctx context.Context, r provider.Request) (string, error) {
	return a.frames.HandleRequest(ctx, r)
}

func (a *app) HandleSession(ctx context.Context, r provider.Request, stream *provider.Stream) error {
	var in struct {
		Direct     *direct.Offer `json:"direct"`
		StreamBody bool          `json:"stream_body"`
	}
	_ = json.Unmarshal([]byte(r.Message), &in)
	note(in.Direct != nil)
	switch {
	case a.mode == "old":
		// The request fields of the HTTP spec before the pipe.
		var old struct {
			Query        string              `json:"query,omitempty"`
			Headers      map[string][]string `json:"headers,omitempty"`
			Body         string              `json:"body,omitempty"`
			BodyBase64   string              `json:"body_base64,omitempty"`
			WebSocket    bool                `json:"websocket,omitempty"`
			StreamBody   bool                `json:"stream_body,omitempty"`
			Subprotocols []string            `json:"subprotocols,omitempty"`
		}
		if strings.TrimSpace(r.Message) != "" && strictjson.Decode(strings.NewReader(r.Message), &old) != nil {
			return provider.ErrInvalidRequest
		}
	case in.Direct == nil:
	case a.mode == "deaf":
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
		if err = json.NewEncoder(stream).Encode(httpadapter.SessionRecord{Type: "direct", SDP: answer}); err != nil {
			return err
		}
		<-ctx.Done()
		return httpadapter.ErrNoPath
	case a.mode == "break":
		pipe, answer, err := direct.Accept(ctx, direct.AcceptConfig{Policy: direct.Auto, Options: direct.Options{Loopback: true}}, *in.Direct)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stream)
		if err = encoder.Encode(httpadapter.SessionRecord{Type: "direct", SDP: answer}); err != nil {
			return err
		}
		if err = pipe.Wait(ctx); err != nil {
			return httpadapter.ErrNoPath
		}
		if in.StreamBody {
			_, _ = io.ReadFull(pipe, make([]byte, 64<<10))
			_ = pipe.Close()
			return httpadapter.ErrPipeFailed
		}
		if err = encoder.Encode(httpadapter.SessionRecord{Type: "response", Status: 200}); err != nil {
			return err
		}
		_, _ = pipe.Write(make([]byte, 64<<10))
		_ = pipe.Close()
		<-ctx.Done()
		return httpadapter.ErrPipeFailed
	}
	return a.frames.HandleSession(ctx, r, stream)
}

// note writes one line to PIPE_HTTP_LOG.
func note(offer bool) {
	line := "frames\n"
	if offer {
		line = "offer\n"
	}
	f, err := os.OpenFile(os.Getenv("PIPE_HTTP_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

func main() {
	body := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(make([]byte, size))
	})
	stdio.Main("pipe-http", func(ctx context.Context, opts provider.Options) error {
		a := &app{frames: httpadapter.NewWithPipes(body, direct.AcceptConfig{Policy: direct.Off}), mode: os.Getenv("PIPE_HTTP_MODE")}
		return provider.Run(ctx, a, opts)
	})
}
