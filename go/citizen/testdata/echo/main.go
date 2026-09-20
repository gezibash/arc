// Command echo is the provider that the tests of the citizen serve. It
// answers with the body that it receives, and fails a body that says "fail".
package main

import (
	"context"
	"os"

	"github.com/gezibash/arc/go/provider"
)

type echo struct{}

func (echo) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	switch request.Message {
	case "fail":
		return "", provider.Error("refused_on_purpose")
	case "panic":
		panic("the provider broke")
	}
	return request.Method() + " " + request.Path() + " " + request.Message, nil
}

func main() {
	if err := provider.Run(context.Background(), echo{}, provider.Options{}); err != nil {
		os.Exit(1)
	}
}
