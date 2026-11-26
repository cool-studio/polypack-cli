#!/bin/sh
# Installs the latest polypack CLI for this machine from PolyPack, checked
# against a SHA256SUMS signed by PolyPack's key (ADR-0037):
#   curl -fsSL https://polypack.io/install.sh | sh
# POLYPACK_VERSION=v0.9.0 picks a version; POLYPACK_INSTALL_DIR a directory.
# The signature is checked with openssl, when it is installed; the checksum
# always is.
set -eu

version="${POLYPACK_VERSION:-latest}"
files="${POLYPACK_CLI_FILES_URL:-https://cool-studio.pkg.polypack.io/files/polypack-cli}"
# The key cli/pinned.go pins (SHA256:+ROG5yRGsHpaitw35aQcW0nNM/1YwDautofcDD7YR/A).
pinned_key="-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE5vdIn5q3cmdzjHv9dcXlOpiLmMEr
lI9zWZRHhXUADQbnCpV2GOOwGPj3laRmIALF/Df7Oa2mAShg9JE94Q4wKg==
-----END PUBLIC KEY-----"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "polypack: unsupported system $(uname -s); download a binary from https://polypack.io/docs/cli" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "polypack: unsupported machine $(uname -m)" >&2; exit 1 ;;
esac

file="polypack-$os-$arch"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if [ "$version" = "latest" ]; then
  version="v$(curl -fsSL "$files/index.json" | sed -n 's/.*"latest":"\([^"]*\)".*/\1/p')"
  [ "$version" != "v" ] || { echo "polypack: PolyPack has no stable release to install." >&2; exit 1; }
fi
base="$files/${version#v}"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"

if command -v openssl >/dev/null 2>&1; then
  curl -fsSL "$base/SHA256SUMS.sig" -o "$tmp/SHA256SUMS.sig.b64"
  printf '%s\n' "$pinned_key" > "$tmp/pinned.pem"
  openssl base64 -d -A -in "$tmp/SHA256SUMS.sig.b64" -out "$tmp/SHA256SUMS.sig"
  if ! openssl dgst -sha256 -verify "$tmp/pinned.pem" -signature "$tmp/SHA256SUMS.sig" "$tmp/SHA256SUMS" >/dev/null 2>&1; then
    echo "polypack: SHA256SUMS from PolyPack is not signed by PolyPack's key; not installing." >&2
    exit 1
  fi
else
  echo "polypack: no openssl, so SHA256SUMS's signature is not checked (the checksum still is)." >&2
fi
curl -fsSL "$base/$file" -o "$tmp/$file"
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
