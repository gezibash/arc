#!/bin/sh
# Use the release's Erlang runtime; there is no compiler or download at startup.
set -eu
provider_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
provider=$(basename "$provider_dir")
exec /app/erts-*/bin/escript "$provider_dir/$provider" "$@"
