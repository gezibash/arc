#!/bin/sh
set -eu
cd "$(dirname "$0")"
if [ ! -x ./releases ] || [ -n "$(find lib mix.exs -newer ./releases 2>/dev/null)" ]; then
  mix deps.get 1>&2
  mix escript.build 1>&2
fi
exec ./releases
