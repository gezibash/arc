package main

import (
	"context"
	"encoding/json"
	execadapter "github.com/gezibash/arc/adapters/exec"
	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/internal/testsession"
	"io"
	"strings"
	"testing"
	"time"
)

func execStream(t *testing.T, s *server, from, body string, mode session.Mode) *session.Stream {
	return testsession.Start(t, mode, func(ctx context.Context, stream *session.Stream) error {
		return s.HandleSession(ctx, provider.Request{From: from, Message: body, Meta: map[string]any{"method": "EXEC"}}, stream)
	})
}
func TestExecSessionInteractiveIOAndExit(t *testing.T) {
	s := testServer(t)
	stream := execStream(t, s, caller, `{"script":"printf ready; read word; printf '%s' \"$word\"; printf problem >&2; exit 7"}`, session.Duplex)
	rd := execadapter.NewReader(stream)
	first, err := rd.Next()
	if err != nil || first.Type != "stdout" || string(first.Data) != "ready" {
		t.Fatalf("no output before input: %+v %v", first, err)
	}
	if err = json.NewEncoder(stream).Encode(execadapter.Record{Type: "stdin", Data: []byte("answer\n")}); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	exit := -1
	for {
		v, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch v.Type {
		case "stdout":
			out.Write(v.Data)
		case "stderr":
			stderr.Write(v.Data)
		case "exit":
			exit = v.Exit
		}
	}
	if out.String() != "answer" || stderr.String() != "problem" || exit != 7 || stream.Wait() != nil {
		t.Fatalf("out=%q err=%q exit=%d", out.String(), stderr.String(), exit)
	}
}
func TestExecSessionTerminalResizeAndEOF(t *testing.T) {
	s := testServer(t)
	stream := execStream(t, s, caller, `{"script":"stty -echo; printf ready; read word; stty size; printf '%s' \"$word\"","pty":true,"rows":24,"cols":80}`, session.Duplex)
	rd := execadapter.NewReader(stream)
	first, err := rd.Next()
	if err != nil || !strings.Contains(string(first.Data), "ready") {
		t.Fatalf("PTY start: %+v %v", first, err)
	}
	enc := json.NewEncoder(stream)
	if err = enc.Encode(execadapter.Record{Type: "resize", Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	if err = enc.Encode(execadapter.Record{Type: "stdin", Data: []byte("terminal\n")}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	exit := -1
	for {
		v, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out.Write(v.Data)
		if v.Type == "exit" {
			exit = v.Exit
		}
	}
	if !strings.Contains(out.String(), "40 100") || !strings.Contains(out.String(), "terminal") || exit != 0 {
		t.Fatalf("PTY: %q exit %d", out.String(), exit)
	}
	pipe := execStream(t, s, caller, `{"argv":["cat"]}`, session.Duplex)
	if err = pipe.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(pipe); err != nil || pipe.Wait() != nil {
		t.Fatalf("EOF: %v", err)
	}
}
func TestExecSessionGrantsLimitsAndCancellation(t *testing.T) {
	s := testServer(t)
	denied := execStream(t, s, stranger, `{"argv":["echo","bad"]}`, session.ServerStream)
	if _, err := io.ReadAll(denied); err == nil || err.Error() != "access_denied" {
		t.Fatalf("grant: %v", err)
	}
	s.config.Limits.OutputBytes = 32
	limit := execStream(t, s, caller, `{"script":"printf '%100s' x"}`, session.ServerStream)
	if _, err := io.ReadAll(limit); err == nil || !strings.Contains(err.Error(), "output_too_large") {
		t.Fatalf("limit: %v", err)
	}
	s.config.Limits.OutputBytes = 1024
	active := execStream(t, s, caller, `{"script":"echo ready; sleep 100"}`, session.ServerStream)
	rd := execadapter.NewReader(active)
	if _, err := rd.Next(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = active.Close()
	if err := active.Wait(); err == nil || time.Since(start) > time.Second {
		t.Fatalf("cancel: %v", err)
	}
}
