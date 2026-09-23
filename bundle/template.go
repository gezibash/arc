package bundle

import (
	"fmt"
	"time"
)

const arcfileTemplate = `version = 1

[runtime]
type = "exec"
command = "./run.sh"
cwd = "."

[manifest]
path = "./manifest.json"
`

// manifestTemplate writes a capability that answers one message. The owner
// edits it to say what the provider really does.
func manifestTemplate(space, name string) string {
	return fmt.Sprintf(`{
  "published_at": %q,
  "release": {
    "version": "0.1.0",
    "channel": "stable"
  },
  "capability": {
    "id": "primary",
    "kind": "service",
    "scheme": %q,
    "title": %q,
    "summary": "A starter ARC app. Edit this manifest and the runtime to say what your capability does.",
    "invocation": {
      "method": "RAW",
      "path": "/"
    },
    "examples": [
      "hello from arc"
    ]
  }
}
`, time.Now().UTC().Format(time.RFC3339), space, name)
}

// interfaceTemplate writes the commands of the capability, as interface
// version 1 defines them. Its one command sends the text of the caller to the
// runtime as the body, and shows the reply.
func interfaceTemplate(space, name string) string {
	return fmt.Sprintf(`{
  "interface": 1, "id": %q, "shape": "service",
  "title": %q,
  "summary": "A starter ARC app. Edit this manifest and the runtime to say what your capability does.",
  "service": {"method": "RAW", "path": "/", "max_bytes": 1048576},
  "kinds": {},
  "formats": {"reply": {"record": "{{content}}"}},
  "commands": [
    {"path": ["say"], "summary": "Send a message, and show the reply",
     "args": [{"name": "message", "kind": "positional", "type": "text", "variadic": true, "required": true}],
     "action": {"call": {"class": "live", "body": "{{message}}"}},
     "output": {"format": "reply"}}
  ]
}
`, space, name)
}

// runtimeTemplate writes a program that reads one request for each line and
// answers it. It is a shell starter. Write a real runtime for real work.
func runtimeTemplate(space string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu

namespace="%s"

extract_field() {
  printf '%%s\n' "$1" | awk -v key="$2" '
    {
      needle = "\"" key "\":\""
      start = index($0, needle)

      if (start > 0) {
        value = substr($0, start + length(needle))
        stop = index(value, "\"")

        if (stop > 0) {
          print substr(value, 1, stop - 1)
        }
      }
    }
  '
}

while IFS= read -r line; do
  request_id="$(extract_field "$line" request_id)"
  message="$(extract_field "$line" message)"

  # A shell starter. It is good enough for hello-world traffic. Write a real
  # runtime for anything that must read JSON correctly.
  if [ -n "$message" ]; then
    reply="hello from $namespace: $message"
  else
    reply="hello from $namespace"
  fi

  if [ -n "$request_id" ]; then
    printf '{"op":"reply","request_id":"%%s","reply":"%%s"}\n' "$request_id" "$reply"
  else
    printf '{"reply":"%%s"}\n' "$reply"
  fi
done
`, space)
}
