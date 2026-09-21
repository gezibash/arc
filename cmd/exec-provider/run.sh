#!/bin/sh
# Run the provider. On a citizen machine, EXEC_PROVIDER names the binary of
# an ARC release. Without it, build the provider from this checkout first.
# The build writes to stderr, because stdout carries the ARC stream.
set -eu
if [ -n "${EXEC_PROVIDER:-}" ]; then
  exec "$EXEC_PROVIDER" "$@"
fi

cd "$(dirname "$0")"
binary="${TMPDIR:-/tmp}/arc-exec-provider"

if [ ! -x "$binary" ] || [ -n "$(find . -name '*.go' -newer "$binary" 2>/dev/null)" ]; then
  go build -o "$binary" . 1>&2
fi

exec "$binary" "$@"
