package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

// A Go http.Handler behind httpadapter.New answers a call that crosses a
// relay. The handler sees the method, the path, the query and the content of
// the call, and the key of the caller. The caller sees the status, the header
// fields and the content of the response. See docs/http/SPEC.md, sections 3
// to 5.
func TestAGoHandlerAnswersACallThroughARelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	provider, providerKey := providerHome(t, relay)
	caller, callerKey := providerHome(t, relay)
	serveSessionProvider(t, ctx, provider, providerBinary(t, "./testdata/http-handler"), "testdata/http-handler/interface.json")
	ok(t, caller, "", "install", providerKey, "--yes")

	// The caller sends a forged Arc-Caller field. The adapter must replace it.
	message := `{"query": "tag=a&limit=10", "headers": {"Arc-Caller": ["` + strings.Repeat("0", 64) + `"]}, "body": "hello over a relay"}`
	out := ok(t, caller, "", "call", "--raw", "--method", "POST", "http+arc://"+providerKey+"/echo", message)

	var reply struct {
		Status  int                 `json:"status"`
		Headers map[string][]string `json:"headers"`
		Body    string              `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("the reply is not one JSON object: %v\n%s", err, out)
	}
	if reply.Status != 201 {
		t.Errorf("status = %d, want 201", reply.Status)
	}
	if got := reply.Headers["Location"]; len(got) != 1 || got[0] != "/echo/7" {
		t.Errorf("Location = %v, want [/echo/7]", got)
	}
	var seen struct{ Caller, Query, Body string }
	if err := json.Unmarshal([]byte(reply.Body), &seen); err != nil {
		t.Fatalf("the content is not what the handler wrote: %v\n%s", err, reply.Body)
	}
	if seen.Caller != callerKey {
		t.Errorf("the handler saw the caller %q, want %q", seen.Caller, callerKey)
	}
	if seen.Query != "tag=a&limit=10" {
		t.Errorf("the handler saw the query %q, want tag=a&limit=10", seen.Query)
	}
	if seen.Body != "hello over a relay" {
		t.Errorf("the handler saw the content %q, want hello over a relay", seen.Body)
	}
}
