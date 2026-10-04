package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/iface"
	httpadapter "github.com/gezibash/arc/sdk/httpadapter"
	"github.com/gezibash/arc/sdk/providertest"
)

func TestHTTPSessionCLIDecodesBodyAndStatus(t *testing.T) {
	stream := providertest.Start(t, session.ServerStream, func(_ context.Context, s *session.Stream) error {
		encoder := json.NewEncoder(s)
		for _, v := range []httpadapter.SessionRecord{{Type: "response", Status: 404}, {Type: "body", Data: []byte("missing")}, {Type: "end"}} {
			if err := encoder.Encode(v); err != nil {
				return err
			}
		}
		return nil
	})
	cmd := sessionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := httpSessionCLI(cmd, stream)
	var exit iface.ExitError
	if !errors.As(err, &exit) || exit.Code != 22 || out.String() != "missing" {
		t.Fatalf("%q %v", out.String(), err)
	}
}
func TestWebSocketCLITextMapping(t *testing.T) {
	stream := providertest.Start(t, session.Duplex, func(_ context.Context, s *session.Stream) error {
		encoder := json.NewEncoder(s)
		if err := encoder.Encode(httpadapter.SessionRecord{Type: "response", Status: 101}); err != nil {
			return err
		}
		record, err := httpadapter.NewSessionReader(s).Next()
		if err != nil {
			return err
		}
		if record.Type != "text" || record.Text != "hello" {
			t.Errorf("input: %+v", record)
		}
		if err = encoder.Encode(record); err != nil {
			return err
		}
		return encoder.Encode(httpadapter.SessionRecord{Type: "ws_close", Code: 1000})
	})
	cmd := sessionCmd()
	cmd.SetIn(strings.NewReader("hello\n"))
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := websocketCLI(cmd, stream); err != nil || out.String() != "hello\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}

func TestHTTPCLIPreservesNestedSessionFailure(t *testing.T) {
	stream := providertest.Start(t, session.ServerStream, func(_ context.Context, s *session.Stream) error {
		encoder := json.NewEncoder(s)
		for _, record := range []httpadapter.SessionRecord{{Type: "response", Status: 200}, {Type: "body", Data: []byte("partial")}, {Type: "trailers", Headers: http.Header{"Arc-Session-Error": {`"refused"`}}}, {Type: "end"}} {
			if err := encoder.Encode(record); err != nil {
				return err
			}
		}
		return nil
	})
	cmd := sessionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := httpSessionCLI(cmd, stream); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("nested failure: %v", err)
	}
}
