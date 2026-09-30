// Command arc-releases runs the releases app service.
package main

import (
	"github.com/gezibash/arc/adapters/provider/stdio"
	"github.com/gezibash/arc/apps/releases/server"
)

func main() { stdio.Main("arc-releases", server.Run) }
