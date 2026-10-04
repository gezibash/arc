package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/iface"
	execadapter "github.com/gezibash/arc/sdk/execadapter"
	"github.com/gezibash/arc/sdk/providertest"
)

func TestExecSessionCLIOutputAndExit(t *testing.T) {
	stream := providertest.Start(t, session.ServerStream, func(_ context.Context, s *session.Stream) error {
		e := json.NewEncoder(s)
		for _, v := range []execadapter.Record{{Type: "stdout", Data: []byte("hello")}, {Type: "stderr", Data: []byte("problem")}, {Type: "exit", Exit: 7}} {
			if err := e.Encode(v); err != nil {
				return err
			}
		}
		return nil
	})
	cmd := sessionCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader(""))
	err := execSession(cmd, stream, false)
	var code iface.ExitError
	if !errors.As(err, &code) || code.Code != 7 || out.String() != "hello" || stderr.String() != "problem" {
		t.Fatalf("%q %q %v", out.String(), stderr.String(), err)
	}
}

// The cases follow the escape of ssh: "~." at the start of a line detaches,
// and "~~" there sends one "~".
func TestDetachEscape(t *testing.T) {
	for _, c := range []struct {
		name   string
		reads  []string
		sent   string
		detach bool
	}{
		{"at the start of the session", []string{"~."}, "", true},
		{"after a carriage return", []string{"ls\r~."}, "ls\r", true},
		{"after a newline", []string{"ls\n~."}, "ls\n", true},
		{"inside a line", []string{"a~."}, "a~.", false},
		{"tilde tilde sends one tilde", []string{"~~."}, "~.", false},
		{"tilde then another key", []string{"~x"}, "~x", false},
		{"sequence split across reads", []string{"\r~", "."}, "\r", true},
		{"held tilde then a key in the next read", []string{"\r~", "q"}, "\r~q", false},
		{"bytes after the sequence are not sent", []string{"~.rest"}, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var escape detachEscape
			var sent strings.Builder
			detach := false
			for _, read := range c.reads {
				out, done := escape.filter([]byte(read))
				sent.Write(out)
				if done {
					detach = true
					break
				}
			}
			if sent.String() != c.sent || detach != c.detach {
				t.Fatalf("sent %q detach %v, want %q %v", sent.String(), detach, c.sent, c.detach)
			}
		})
	}
}
