#!/bin/sh
# Build the service program. Diagnostics go to stderr; stdout carries ARC.
set -eu
if [ -n "${EXEC_PROVIDER:-}" ]; then
  exec "$EXEC_PROVIDER" "$@"
fi
cd "$(dirname "$0")"
binary="${TMPDIR:-/tmp}/arc-exec"
go build -o "$binary" ../../cmd/arc-exec 1>&2
exec "$binary" "$@"
