// Command pipe-http is an HTTP provider for the tests of the pipe. Each
// session sends 1 MiB. PIPE_HTTP_MODE picks the behavior:
//
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

	"github.com/gezibash/arc/sdk/direct"
	httpadapter "github.com/gezibash/arc/sdk/httpadapter"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/stdio"
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
	case in.Direct == nil:
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
