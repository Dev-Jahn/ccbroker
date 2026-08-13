#!/bin/sh
# install.sh — install the ccbroker client (ccb).
#   curl -fsSL https://raw.githubusercontent.com/Dev-Jahn/ccbroker/main/install.sh | sh
#
# Layout: the real binary always lands in ~/.config/ccbroker/bin/ccb, and the
# install dir gets a symlink to it. Everything that has to name ccb by path (the
# launchd/systemd unit from `ccb setup`, the statusline, the Claude Code plugin's
# updater) points at that one file, so later updates replace it in place and no
# unit file or PATH entry ever needs editing.
#
# Env overrides:
#   CCB_VERSION      release tag to install (default: latest)
#   CCB_INSTALL_DIR  directory for the symlink (default: $HOME/.local/bin, or
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
stage=""
link=""
cleanup() {
  rm -rf "$tmp"
  [ -n "$stage" ] && rm -f "$stage"
  [ -n "$link" ] && rm -f "$link"
  return 0
}
trap cleanup EXIT

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

# The canonical binary: one real file, replaced in place by every later update.
managed_dir="$HOME/.config/ccbroker/bin"
mkdir -p "$managed_dir"
managed_dir=$(cd "$managed_dir" && pwd -P)
managed="$managed_dir/ccb"

# Stage in the destination directory so the swap is a same-filesystem rename:
# atomic, and safe while an older ccb is still running (a cross-device move
# would truncate the file out from under it).
stage="$managed_dir/.ccb.new.$$"
cp "$tmp/$ASSET" "$stage"
chmod +x "$stage"
mv -f "$stage" "$managed"
stage=""

if [ -n "${CCB_INSTALL_DIR:-}" ]; then
  dir="$CCB_INSTALL_DIR"
elif [ "$(id -u)" = 0 ]; then
  dir=/usr/local/bin
else
  dir="$HOME/.local/bin"
fi

mkdir -p "$dir"
dir=$(cd "$dir" && pwd -P)

echo "installed ccb -> $managed"
if [ "$dir/ccb" = "$managed" ]; then
  echo "$dir is the managed directory; no symlink needed"
else
  if [ -d "$dir/ccb" ] && [ ! -L "$dir/ccb" ]; then
    echo "$dir/ccb is a directory; move it aside and re-run" >&2
    exit 1
  fi
  # Link under a temp name, then rename over whatever is there (an older real
  # binary, or a stale link): one step, and $dir/ccb is never missing.
  link="$dir/.ccb.lnk.$$"
  ln -sf "$managed" "$link"
  mv -f "$link" "$dir/ccb"
  link=""
  echo "linked $dir/ccb -> $managed"
fi

"$dir/ccb" version
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "warning: $dir is not in your PATH; add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
esac
echo "Next: run 'ccb setup'"
