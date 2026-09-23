package bundle

import (
	"encoding/json"
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
  "published_at": %s,
  "release": {
    "version": "0.1.0",
    "channel": "stable"
  },
  "capability": {
    "id": "primary",
    "kind": "service",
    "scheme": %s,
    "title": %s,
    "summary": "A starter ARC app. Edit this manifest and the runtime to say what your capability does.",
    "invocation": {
      "method": "RAW",
      "path": "/"
    },
    "examples": [
      "hello from arc"
    ]
  },
  "interfaces": {
    "cli": {
      "version": 1,
      "namespace": %s,
      "summary": "The commands of the starter ARC app",
      "commands": [
        {
          "path": [],
          "summary": "Send a message to the starter ARC app",
          "args": [
            {
              "name": "message",
              "kind": "positional",
              "required": false,
              "variadic": true,
              "description": "The text to send"
            }
          ],
          "input": {
            "source": "template",
            "template": "{{message}}"
          },
          "examples": [
            "hello from arc"
          ]
        }
      ]
    }
  }
}
`, jsonString(time.Now().UTC().Format(time.RFC3339)), jsonString(space), jsonString(name), jsonString(space))
}

// jsonString writes a string as a JSON string. Go quoting (%q) is not JSON:
// it writes a byte that is not UTF-8 as \xa9, and JSON has no such escape.
func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
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
