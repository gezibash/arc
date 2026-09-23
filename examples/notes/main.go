// Command notes is an example of HTTP over ARC in one program. provider.HTTP
// serves the notes service in this process, as the capability http. The
// service keeps its notes in SQLite over ARC: it calls a sqlite capability
// that its citizen installed, through provider.Caller.
//
// NOTES_DB names the database, for example sqlite+arc://<provider>/main.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gezibash/arc/examples/notes/service"
	"github.com/gezibash/arc/provider"
)

// app serves the notes service, and holds the caller that reaches the
// database.
type app struct {
	web    provider.Handler
	caller provider.Caller
}

func (a *app) SetCaller(caller provider.Caller) { a.caller = caller }

func (a *app) HandleRequest(ctx context.Context, r provider.Request) (string, error) {
	return a.web.HandleRequest(ctx, r)
}

func main() {
	address := os.Getenv("NOTES_DB")
	if address == "" {
		fmt.Fprintln(os.Stderr, "notes: NOTES_DB must name a database: sqlite+arc://<provider>/<name>")
		os.Exit(1)
	}
	a := &app{}
	a.web = provider.HTTP(service.Handler(func(ctx context.Context, body string) (string, error) {
		return a.caller.Call(ctx, address, body)
	}))

	if err := provider.Run(context.Background(), a, provider.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
