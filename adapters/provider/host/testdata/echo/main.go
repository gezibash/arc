// Command echo is the provider that the tests of the citizen serve. It
// answers with the body that it receives, and fails a body that says "fail".
// A body "call <address> <body>" calls that capability through the host, and
// answers with what came back. A body "env <name>" answers with that
// environment variable. With ECHO_LOG, it appends the body of each request
// to that file, one line each, so that a test counts each run.
package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/stdio"
)

type echo struct {
	caller   provider.Caller
	sessions provider.SessionCaller
}

func (e *echo) SetCaller(caller provider.Caller) { e.caller = caller }

func (e *echo) HandleRequest(ctx context.Context, request provider.Request) (string, error) {
	if path := os.Getenv("ECHO_LOG"); path != "" {
		if file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, _ = file.WriteString(request.Message + "\n")
			_ = file.Close()
		}
	}
	switch request.Message {
	case "fail":
		return "", provider.Error("refused_on_purpose")
	case "panic":
		panic("the provider broke")
	}
	if name, ok := strings.CutPrefix(request.Message, "env "); ok {
		return os.Getenv(name), nil
	}
	if rest, ok := strings.CutPrefix(request.Message, "call "); ok {
		address, body, _ := strings.Cut(rest, " ")
		reply, err := e.caller.Call(ctx, address, body)
		var failed *provider.CallError
		switch {
		case errors.As(err, &failed) && failed.Refused:
			return "refused: " + failed.Reason, nil
		case err != nil:
			return "failed: " + err.Error(), nil
		}
		return "reply: " + reply, nil
	}
	return request.Method() + " " + request.Path() + " " + request.Message, nil
}

func main() {
	if err := stdio.Run(context.Background(), &echo{}, provider.Options{}); err != nil {
		os.Exit(1)
	}
}
