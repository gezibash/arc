// Command muterelay is a relay that takes each WebSocket connection and
// never answers on it: it reads and drops each message. A proof uses it as
// the most common black hole, a relay that connects and then says nothing.
// It listens on MUTERELAY_LISTEN, and prints its URL.
//
// With MUTERELAY_NOTICE it sends that NOTICE first on each connection, and
// with MUTERELAY_MODE=all it answers each message as an empty relay.
// With MUTERELAY_MODE=mail it answers each message except a read of mail
// (kind 1059, or a filter on #p): a relay that is silent for one kind. It
// has no events, so a REQ gets EOSE, an EVENT gets OK, and a NEG-OPEN gets
// NEG-ERR. With MUTERELAY_MODE=closed it refuses each message with the
// reason "blocked: ...", with MUTERELAY_MODE=auth with "auth-required:
// ...", and with MUTERELAY_MODE=403 it refuses the WebSocket upgrade with
// HTTP 403. MUTERELAY_REASON replaces the reason of a refusal, and with
// MUTERELAY_MODE=mailclosed it answers as with mail, but refuses a read of
// mail with that reason. With MUTERELAY_MODE=quiet it answers as an empty
// relay until it took MUTERELAY_QUIET_AFTER events, and then answers
// nothing, on each connection: a relay that stops answering partway
// through a sync. With MUTERELAY_ACCEPTS it writes one line to that file
// for each connection that it takes.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync/atomic"

	"github.com/coder/websocket"
)

func main() {
	l, err := net.Listen("tcp", os.Getenv("MUTERELAY_LISTEN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "muterelay:", err)
		os.Exit(2)
	}
	fmt.Printf("ws://%s\n", l.Addr())
	mode := os.Getenv("MUTERELAY_MODE")
	quietAfter, _ := strconv.Atoi(os.Getenv("MUTERELAY_QUIET_AFTER"))
	var took atomic.Int32
	reason := cmp.Or(os.Getenv("MUTERELAY_REASON"), "blocked: this relay serves no one")
	text := os.Getenv("MUTERELAY_NOTICE")
	accepts := os.Getenv("MUTERELAY_ACCEPTS")
	_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "403" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
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
		if text != "" {
			body, _ := json.Marshal([]any{"NOTICE", text})
			_ = conn.Write(ctx, websocket.MessageText, body)
		}
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var answer []byte
			switch mode {
			case "mail":
				answer = answerAllButMail(msg)
			case "closed":
				answer = refuse(msg, reason)
			case "mailclosed":
				if answer = answerAllButMail(msg); answer == nil {
					answer = refuse(msg, reason)
				}
			case "all":
				if answer = answerAllButMail(msg); answer == nil {
					answer = endOfStored(msg)
				}
			case "auth":
				answer = refuse(msg, "auth-required: this relay serves only its members")
			case "quiet":
				if int(took.Load()) < quietAfter {
					answer = answerAll(msg)
					if answer != nil && bytes.HasPrefix(answer, []byte(`["OK"`)) {
						took.Add(1)
					}
				}
			}
			if answer != nil {
				_ = conn.Write(ctx, websocket.MessageText, answer)
			}
			if answer != nil {
				_ = conn.Write(ctx, websocket.MessageText, answer)
			}
		}
	}))
}

// answerAll is the answer of an empty relay to one message: EOSE to a REQ,
// NEG-ERR to a NEG-OPEN, and OK to an EVENT.
func answerAll(msg []byte) []byte {
	var parts []json.RawMessage
	if json.Unmarshal(msg, &parts) != nil || len(parts) < 2 {
		return nil
	}
	var verb, sub string
	_ = json.Unmarshal(parts[0], &verb)
	_ = json.Unmarshal(parts[1], &sub)
	var answer []any
	switch verb {
	case "REQ":
		answer = []any{"EOSE", sub}
	case "NEG-OPEN":
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

// endOfStored is EOSE for a REQ, or nil for each other message.
func endOfStored(msg []byte) []byte {
	var parts []json.RawMessage
	if json.Unmarshal(msg, &parts) != nil || len(parts) < 2 {
		return nil
	}
	var verb, sub string
	_ = json.Unmarshal(parts[0], &verb)
	_ = json.Unmarshal(parts[1], &sub)
	if verb != "REQ" {
		return nil
	}
	body, _ := json.Marshal([]any{"EOSE", sub})
	return body
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

// refuse is the refusal of one message with reason, or nil for a message
// that needs no answer.
func refuse(msg []byte, reason string) []byte {
	var parts []json.RawMessage
	if json.Unmarshal(msg, &parts) != nil || len(parts) < 2 {
		return nil
	}
	var verb, sub string
	_ = json.Unmarshal(parts[0], &verb)
	_ = json.Unmarshal(parts[1], &sub)
	var answer []any
	switch verb {
	case "REQ", "COUNT":
		answer = []any{"CLOSED", sub, reason}
	case "NEG-OPEN":
		answer = []any{"NEG-ERR", sub, reason}
	case "EVENT":
		var event struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(parts[1], &event)
		answer = []any{"OK", event.ID, false, reason}
	default:
		return nil
	}
	body, _ := json.Marshal(answer)
	return body
}
