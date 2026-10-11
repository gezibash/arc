#!/bin/sh
# Build the service program. Diagnostics go to stderr; stdout carries ARC.
set -eu
cd "$(dirname "$0")"
binary="${TMPDIR:-/tmp}/arc-storage"
go build -o "$binary" ./cmd/arc-storage 1>&2
exec "$binary" "$@"
