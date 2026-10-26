// Command muterelay is a relay that takes each WebSocket connection and
// never answers on it: it reads and drops each message. A proof uses it as
// the most common black hole, a relay that connects and then says nothing.
// It listens on MUTERELAY_LISTEN, and prints its URL.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/coder/websocket"
)

func main() {
	l, err := net.Listen("tcp", os.Getenv("MUTERELAY_LISTEN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "muterelay:", err)
		os.Exit(2)
	}
	fmt.Printf("ws://%s\n", l.Addr())
	_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := context.Background()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
}
