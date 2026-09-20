#!/bin/sh
# Build the provider, then run it. The build writes to stderr, because
# stdout carries the ARC stream.
set -eu
cd "$(dirname "$0")"
binary="${TMPDIR:-/tmp}/arc-dm-provider"

if [ ! -x "$binary" ] || [ -n "$(find . -name '*.go' -newer "$binary" 2>/dev/null)" ]; then
  go build -o "$binary" . 1>&2
fi

exec "$binary" "$@"
