#!/bin/sh
# Build arc-http and this server, then let arc-http run the
# server. The builds write to stderr, because stdout carries the ARC stream.
set -eu
cd "$(dirname "$0")"
adapter="${TMPDIR:-/tmp}/arc-http"
server="${TMPDIR:-/tmp}/arc-notes-server"

go build -o "$adapter" ../../cmd/arc-http 1>&2
go build -o "$server" . 1>&2

exec "$adapter" "$server"
