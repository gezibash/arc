// Command arc-exec runs the exec app service.
package main

import (
	"github.com/gezibash/arc/adapters/provider/stdio"
	"github.com/gezibash/arc/apps/exec/server"
)

func main() { stdio.Main("arc-exec", server.Run) }
