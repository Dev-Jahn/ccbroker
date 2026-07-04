#!/bin/sh
# install.sh — install the ccbroker client (ccb).
#   curl -fsSL https://raw.githubusercontent.com/Dev-Jahn/ccbroker/main/install.sh | sh
#
# Env overrides:
#   CCB_VERSION      release tag to install (default: latest)
#   CCB_INSTALL_DIR  install directory (default: $HOME/.local/bin, or
#                    /usr/local/bin when run as root)
set -eu

BASE="https://github.com/Dev-Jahn/ccbroker/releases"

os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "unsupported OS: $os — see $BASE" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported arch: $arch — see $BASE" >&2; exit 1 ;;
esac

VERSION="${CCB_VERSION:-latest}"
ASSET="ccb_${os}_${arch}"
if [ "$VERSION" = latest ]; then
  URL="$BASE/latest/download/$ASSET"
  CK="$BASE/latest/download/checksums.txt"
else
  URL="$BASE/download/$VERSION/$ASSET"
  CK="$BASE/download/$VERSION/checksums.txt"
fi

dl() {
  # dl <url> <output>
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    echo "need curl or wget to download $1" >&2
    exit 1
  fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

dl "$URL" "$tmp/$ASSET"
dl "$CK" "$tmp/checksums.txt"

want=$(awk -v f="$ASSET" '$2 == f {print $1}' "$tmp/checksums.txt")
[ -n "$want" ] || { echo "no checksum for $ASSET in checksums.txt" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$ASSET" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$ASSET" | awk '{print $1}')
else
  echo "need sha256sum or shasum to verify the download" >&2
  exit 1
fi
[ "$want" = "$got" ] || { echo "checksum mismatch for $ASSET" >&2; exit 1; }

if [ -n "${CCB_INSTALL_DIR:-}" ]; then
  dir="$CCB_INSTALL_DIR"
elif [ "$(id -u)" = 0 ]; then
  dir=/usr/local/bin
else
  dir="$HOME/.local/bin"
fi

mkdir -p "$dir"
mv "$tmp/$ASSET" "$dir/ccb"
chmod +x "$dir/ccb"

echo "installed ccb -> $dir/ccb"
"$dir/ccb" version
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "warning: $dir is not in your PATH; add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
esac
echo "Next: run 'ccb setup'"
