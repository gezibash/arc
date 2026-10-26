// Command muterelay is a relay that takes each WebSocket connection and
// never answers on it: it reads and drops each message. A proof uses it as
// the most common black hole, a relay that connects and then says nothing.
// It listens on MUTERELAY_LISTEN, and prints its URL.
//
// With MUTERELAY_MODE=mail it answers each message except a read of mail
// (kind 1059, or a filter on #p): a relay that is silent for one kind. It
// has no events, so a REQ gets EOSE, an EVENT gets OK, and a NEG-OPEN gets
// NEG-ERR. With MUTERELAY_ACCEPTS it writes one line to that file for each
// connection that it takes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"

	"github.com/coder/websocket"
)

func main() {
	l, err := net.Listen("tcp", os.Getenv("MUTERELAY_LISTEN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "muterelay:", err)
		os.Exit(2)
	}
	fmt.Printf("ws://%s\n", l.Addr())
	mail := os.Getenv("MUTERELAY_MODE") == "mail"
	accepts := os.Getenv("MUTERELAY_ACCEPTS")
	_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		if accepts != "" {
			if f, err := os.OpenFile(accepts, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, _ = f.WriteString("accept\n")
				_ = f.Close()
			}
		}
		ctx := context.Background()
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if mail {
				if answer := answerAllButMail(msg); answer != nil {
					_ = conn.Write(ctx, websocket.MessageText, answer)
				}
			}
		}
	}))
}

// filter is the part of a filter that says whether it reads mail.
type filter struct {
	Kinds []int    `json:"kinds"`
	P     []string `json:"#p"`
}

func (f filter) readsMail() bool {
	return slices.Contains(f.Kinds, 1059) || len(f.P) > 0 && len(f.Kinds) == 0
}

// answerAllButMail is the answer to one message, or nil for a read of mail
// and for a message that needs no answer.
func answerAllButMail(msg []byte) []byte {
	var parts []json.RawMessage
	if json.Unmarshal(msg, &parts) != nil || len(parts) < 2 {
		return nil
	}
	var verb, sub string
	_ = json.Unmarshal(parts[0], &verb)
	_ = json.Unmarshal(parts[1], &sub)
	mailIn := func(raw []json.RawMessage) bool {
		for _, r := range raw {
			var f filter
			if json.Unmarshal(r, &f) == nil && f.readsMail() {
				return true
			}
		}
		return false
	}
	var answer []any
	switch verb {
	case "REQ":
		if mailIn(parts[2:]) {
			return nil
		}
		answer = []any{"EOSE", sub}
	case "NEG-OPEN":
		if mailIn(parts[2:3]) {
			return nil
		}
		answer = []any{"NEG-ERR", sub, "blocked: this relay has no negentropy"}
	case "EVENT":
		var event struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(parts[1], &event)
		answer = []any{"OK", event.ID, true, ""}
	default:
		return nil
	}
	body, _ := json.Marshal(answer)
	return body
}
