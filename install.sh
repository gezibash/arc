#!/bin/sh
# Install arc from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/gezibash/arc/main/install.sh | sh
#
# Environment:
#   ARC_VERSION      Version to install, for example 0.2.1. Default: latest release.
#   ARC_INSTALL_DIR  Where the release unpacks. Default: ~/.local/share/arc
#   ARC_BIN_DIR      Where the arc symlink goes. Default: ~/.local/bin
#
# The script downloads the tarball for this OS and CPU, checks it against
# SHA256SUMS, unpacks it, and links bin/arc into ARC_BIN_DIR.

set -eu

REPO="gezibash/arc"
INSTALL_DIR="${ARC_INSTALL_DIR:-$HOME/.local/share/arc}"
BIN_DIR="${ARC_BIN_DIR:-$HOME/.local/bin}"

fail() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

need curl
need tar

os="$(uname -s)"
arch="$(uname -m)"

case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS: $os" ;;
esac

case "$arch" in
  x86_64 | amd64) arch=x86_64 ;;
  aarch64 | arm64) arch=aarch64 ;;
  *) fail "unsupported CPU: $arch" ;;
esac

target="$os-$arch"
case "$target" in
  linux-x86_64 | linux-aarch64 | darwin-aarch64) ;;
  *) fail "no release for $target" ;;
esac

version="${ARC_VERSION:-}"
if [ -z "$version" ]; then
  version="$(
    curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
      sed -nE 's/^ *"tag_name": *"v([^"]+)".*$/\1/p' |
      head -1
  )"
  [ -n "$version" ] || fail "could not find the latest release"
fi
version="${version#v}"

name="arc-$version-$target"
base="https://github.com/$REPO/releases/download/v$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf 'Downloading %s\n' "$name.tar.gz"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"

expected="$(grep " $name.tar.gz\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)"
[ -n "$expected" ] || fail "$name.tar.gz is not listed in SHA256SUMS"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)"
else
  actual="$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $name.tar.gz"

printf 'Installing to %s\n' "$INSTALL_DIR"
mkdir -p "$tmp/unpack"
tar -xzf "$tmp/$name.tar.gz" -C "$tmp/unpack"
rm -rf "$INSTALL_DIR"
mkdir -p "$(dirname "$INSTALL_DIR")"
mv "$tmp/unpack/arc" "$INSTALL_DIR"

mkdir -p "$BIN_DIR"
ln -sf "$INSTALL_DIR/bin/arc" "$BIN_DIR/arc"

"$BIN_DIR/arc" version

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    printf '\nAdd %s to your PATH:\n' "$BIN_DIR"
    # shellcheck disable=SC2016
    printf '  export PATH="%s:$PATH"\n' "$BIN_DIR"
    ;;
esac
