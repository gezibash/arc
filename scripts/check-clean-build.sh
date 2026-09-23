#!/bin/bash
# Proves that a release build of a clean commit stamps vcs.modified=false.
# It clones the commit, runs the "Build" step of .github/workflows/release.yml
# as it is, and builds the build stage of each Dockerfile. Then it reads the
# stamp of each arc binary with `go version -m`. If one binary shows
# vcs.modified=true, the script fails.
#
#     mise run check-clean-build          # checks HEAD
#     mise run check-clean-build -- v0.15.4
#
# The script checks a commit, so uncommitted changes have no effect. It needs
# git, go, yq and docker.
set -euo pipefail

ref="${1:-HEAD}"
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# A CI runner has no global git config. A global ignore file that lists dist/
# hides the fault, so do not use the global config.
export GIT_CONFIG_GLOBAL=/dev/null

commit="$(git -C "$root" rev-parse "$ref^{commit}")"
git clone -q "$root" "$work/arc"
git -C "$work/arc" checkout -q "$commit"

# The mise shims refuse a mise.toml that is not trusted. The clone is this
# repository, so trust it.
export MISE_TRUSTED_CONFIG_PATHS="$work/arc"

failed=0
check() {
  local label="$1" stamp="$2"
  printf '%-40s %s\n' "$label" "$stamp"
  [ "$stamp" = "vcs.modified=false" ] || failed=1
}

# The tarballs.
yq '.jobs.tarball.steps[] | select(.name == "Build") | .run' \
  "$work/arc/.github/workflows/release.yml" > "$work/build.sh"
(cd "$work/arc" && VERSION=0.0.0-check bash "$work/build.sh")
for tarball in "$work"/arc/dist/*.tar.gz; do
  out="$work/x/$(basename "$tarball")"
  mkdir -p "$out"
  tar -C "$out" -xzf "$tarball"
  check "$(basename "$tarball")" "$(go version -m "$out/arc/bin/arc" | grep -o 'vcs\.modified=.*')"
done

# The images. The release builds them from a fresh checkout, with no dist/.
rm -rf "$work/arc/dist"
for dockerfile in Dockerfile docker/fly-nostr/Dockerfile; do
  image="arc-check-clean-build:$(echo "$dockerfile" | tr '/A-Z' '-a-z')"
  docker build -q --target build -f "$work/arc/$dockerfile" \
    --build-arg VERSION=0.0.0-check -t "$image" "$work/arc" > /dev/null
  check "$dockerfile" "$(docker run --rm "$image" go version -m /out/arc | grep -o 'vcs\.modified=.*')"
  docker rmi -f "$image" > /dev/null
done

if [ "$failed" -ne 0 ]; then
  echo "FAIL: a release build of $commit stamps vcs.modified=true" >&2
  exit 1
fi
echo "ok: each release build of $commit stamps vcs.modified=false"
