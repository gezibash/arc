#!/bin/sh
# stdout is reserved for ARC's newline-delimited JSON protocol.
set -eu
cd "$(dirname "$0")"
exec mise exec -- python -m sqlite_provider
