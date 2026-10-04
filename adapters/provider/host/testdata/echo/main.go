// Command echo is the provider that the tests of the citizen serve. It
// answers with the body that it receives, and fails a body that says "fail".
// A body "call <address> <body>" calls that capability through the host, and
// answers with what came back. A body "env <name>" answers with that
// environment variable.
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
