// Command arc-sqlite runs the sqlite app service.
package main

import (
	"github.com/gezibash/arc/apps/sqlite/server"
	"github.com/gezibash/arc/sdk/stdio"
)

func main() { stdio.Main("arc-sqlite", server.Run) }
