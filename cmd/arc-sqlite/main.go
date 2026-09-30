// Command arc-sqlite runs the sqlite app service.
package main

import (
	"github.com/gezibash/arc/adapters/provider/stdio"
	"github.com/gezibash/arc/apps/sqlite/server"
)

func main() { stdio.Main("arc-sqlite", server.Run) }
