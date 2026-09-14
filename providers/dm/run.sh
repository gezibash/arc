#!/bin/sh
# Starts the dm provider. Builds the escript on first run.
# Build output goes to stderr so stdout stays a clean JSON line stream.
set -eu
cd "$(dirname "$0")"
if [ ! -x ./dm ] || [ -n "$(find lib mix.exs -newer ./dm 2>/dev/null)" ]; then
  mix deps.get 1>&2
  mix escript.build 1>&2
fi
exec ./dm
