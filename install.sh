#!/bin/sh
# Installs the latest polypack CLI for this machine, checksum-verified.
#   curl -fsSL https://raw.githubusercontent.com/cool-studio/polypack-cli/main/install.sh | sh
# POLYPACK_VERSION=v0.1.0 picks a version; POLYPACK_INSTALL_DIR a directory.
set -eu

repo="cool-studio/polypack-cli"
version="${POLYPACK_VERSION:-latest}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "polypack: unsupported system $(uname -s); download a binary from https://github.com/$repo/releases" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "polypack: unsupported machine $(uname -m)" >&2; exit 1 ;;
esac

if [ "$version" = "latest" ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/$version"
fi
file="polypack-$os-$arch"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/$file" -o "$tmp/$file"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"

expected="$(grep " $file\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$file" | cut -d' ' -f1)"
else
  actual="$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1)"
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "polypack: checksum mismatch for $file; not installing." >&2
  exit 1
fi

dir="${POLYPACK_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/$file" "$dir/polypack"
echo "polypack installed at $dir/polypack"
case ":$PATH:" in *":$dir:"*) ;; *) echo "Add $dir to your PATH to run it." ;; esac
echo "Next: polypack login"
