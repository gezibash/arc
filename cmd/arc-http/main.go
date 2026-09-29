// Command arc-http runs the http app service.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gezibash/arc/apps/http/server"
	"github.com/gezibash/arc/core/provider"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: arc-http <program> [args...]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, os.Args[1:], provider.Options{In: os.Stdin, Out: os.Stdout, Log: os.Stderr}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "arc-http:", err)
		os.Exit(1)
	}
}
