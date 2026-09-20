#!/bin/sh
# Run the provider binary of this bundle. The image holds it already, so
# nothing builds or downloads at startup.
set -eu
provider_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
provider=$(basename "$provider_dir")
exec "/app/$provider-provider" "$@"
