#!/bin/sh
# Build http-provider and this server, then let http-provider run the
# server. The builds write to stderr, because stdout carries the ARC stream.
set -eu
cd "$(dirname "$0")"
adapter="${TMPDIR:-/tmp}/arc-http-provider"
server="${TMPDIR:-/tmp}/arc-notes-server"

go build -o "$adapter" ../../cmd/http-provider 1>&2
go build -o "$server" . 1>&2

exec "$adapter" "$server"
