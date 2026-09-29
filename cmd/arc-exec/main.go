// Command arc-exec runs the exec app service.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gezibash/arc/apps/exec/server"
	"github.com/gezibash/arc/core/provider"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, provider.Options{In: os.Stdin, Out: os.Stdout, Log: os.Stderr}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "arc-exec:", err)
		os.Exit(1)
	}
}
