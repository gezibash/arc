// Command bytes is a runtime that carries bytes, for the tests of the
// citizen. It speaks the runtime protocol itself: each request body comes as
// base64, and it answers with the bytes in reverse order, as base64. A body
// of the one byte 0x01 gets a reply that is not base64.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"os"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	out := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event["op"] != "request" {
			continue
		}
		id, _ := event["request_id"].(string)

		message, _ := event["message"].(string)
		body, err := base64.StdEncoding.DecodeString(message)
		if event["encoding"] != "base64" || err != nil {
			out.Encode(map[string]any{"op": "reply", "request_id": id, "error": "the request is not base64"})
			continue
		}

		if len(body) == 1 && body[0] == 0x01 {
			out.Encode(map[string]any{"op": "reply", "request_id": id, "encoding": "base64", "reply": "not base64!"})
			continue
		}

		reversed := make([]byte, len(body))
		for i, b := range body {
			reversed[len(body)-1-i] = b
		}
		out.Encode(map[string]any{
			"op": "reply", "request_id": id, "encoding": "base64",
			"reply": base64.StdEncoding.EncodeToString(reversed),
		})
	}
}
