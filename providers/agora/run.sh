#!/bin/sh
# Build output stays on stderr; stdout is reserved for the ARC JSON stream.
set -eu
cd "$(dirname "$0")"
if [ ! -x ./agora ] || [ -n "$(find lib mix.exs -newer ./agora 2>/dev/null)" ]; then
  mix deps.get 1>&2
  mix escript.build 1>&2
fi
exec ./agora
