// Command arc-http runs the http app service.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gezibash/arc/apps/http/server"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/stdio"
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
