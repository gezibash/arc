// Command arc-exec runs the exec app service.
package main

import (
	"github.com/gezibash/arc/apps/exec/server"
	"github.com/gezibash/arc/sdk/stdio"
)

func main() { stdio.Main("arc-exec", server.Run) }
