// Package stdio connects the provider runtime to process standard streams.
package stdio

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gezibash/arc/sdk/provider"
)

// Main runs an app service program on the process streams. An interrupt or
// SIGTERM stops it cleanly. Any other error goes to standard error, prefixed
// with name, and the process exits with status 1.
func Main(name string, run func(context.Context, provider.Options) error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, provider.Options{In: os.Stdin, Out: os.Stdout, Log: os.Stderr})
	stopped := ctx.Err() != nil
	stop()
	if err != nil && !stopped {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}

// Run supplies process streams, then runs the shared provider protocol.
func Run(ctx context.Context, handler provider.Handler, opts provider.Options) error {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	return provider.Run(ctx, handler, opts)
}
