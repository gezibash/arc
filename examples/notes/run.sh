#!/bin/sh
# Build the example, then run it. The build writes to stderr, because
# stdout carries the ARC stream.
set -eu
cd "$(dirname "$0")"
binary="${TMPDIR:-/tmp}/arc-notes-example"

go build -o "$binary" . 1>&2

exec "$binary" "$@"
