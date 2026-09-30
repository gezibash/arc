// Command arc-releases runs the releases app service.
package main

import (
	"github.com/gezibash/arc/apps/releases/server"
	"github.com/gezibash/arc/sdk/stdio"
)

func main() { stdio.Main("arc-releases", server.Run) }
