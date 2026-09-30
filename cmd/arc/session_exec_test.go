package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	execadapter "github.com/gezibash/arc/adapters/exec"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/internal/testsession"
	"github.com/gezibash/arc/runtime/iface"
	"strings"
	"testing"
)

func TestExecSessionCLIOutputAndExit(t *testing.T) {
	stream := testsession.Start(t, session.ServerStream, func(_ context.Context, s *session.Stream) error {
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
