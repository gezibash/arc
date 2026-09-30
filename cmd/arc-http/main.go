// Command arc-http runs the http app service.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gezibash/arc/adapters/provider/stdio"
	"github.com/gezibash/arc/apps/http/server"
	"github.com/gezibash/arc/core/provider"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: arc-http <program> [args...]")
		os.Exit(2)
	}
	stdio.Main("arc-http", func(ctx context.Context, opts provider.Options) error {
		return server.Run(ctx, os.Args[1:], opts)
	})
}
