// Package stdio connects the provider runtime to process standard streams.
package stdio

import (
	"context"
	"os"

	"github.com/gezibash/arc/core/provider"
)

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
