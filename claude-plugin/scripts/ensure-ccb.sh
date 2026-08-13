#!/bin/sh
# ensure-ccb.sh — keep the ccb binary at the plugin's version.
#
# Run by the plugin's SessionStart hook as
#   "${CLAUDE_PLUGIN_ROOT}/scripts/ensure-ccb.sh"
#
# The plugin version in .claude-plugin/plugin.json is the single version number:
# it names both the plugin and the ccbroker release tag whose binary belongs with
# it. This script keeps ONE real file — ~/.config/ccbroker/bin/ccb — at that
# version. Everything else (the `ccb` on PATH, the launchd/systemd unit, the
# statusline command) is expected to be a symlink to that file, so a plugin
# update never has to touch a unit file or a PATH entry.
#
# Nothing lives under ${CLAUDE_PLUGIN_ROOT}: that path carries the plugin
# version and is swept a couple of weeks after every update.
#
# It never fails a session: every path exits 0. Failures print one line to stderr
# and are recorded in ~/.config/ccbroker/selfupdate.log.
#
# Overridable for testing: HOME (install root), CLAUDE_PLUGIN_ROOT (which
# manifest supplies the desired version).
set -u

REPO="https://github.com/Dev-Jahn/ccbroker"
BASE="$REPO/releases"

home=${HOME:-}
if [ -z "$home" ]; then
	echo "ccb self-update: HOME is unset; skipped" >&2
	exit 0
fi

CCB_DIR="$home/.config/ccbroker"
BIN_DIR="$CCB_DIR/bin"
BIN="$BIN_DIR/ccb"
LOG="$CCB_DIR/selfupdate.log"
LOCK="$CCB_DIR/.selfupdate.lock"

tmpd=""
locked=no

log() {
	mkdir -p "$CCB_DIR" 2>/dev/null || return 0
	printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >>"$LOG" 2>/dev/null || return 0
}

# say prints one line to stderr (SessionStart stdout would land in the session
# context) and records the same line in the log.
say() {
	echo "ccb self-update: $*" >&2
	log "$*"
}

cleanup() {
	[ -n "$tmpd" ] && rm -rf "$tmpd"
	[ "$locked" = yes ] && rmdir "$LOCK" 2>/dev/null
	return 0
}

# bail records a failure and leaves the session alone (exit 0 by contract).
bail() {
	say "FAILED: $*"
	cleanup
	exit 0
}

# ---- desired version: the plugin manifest ----------------------------------

root=${CLAUDE_PLUGIN_ROOT:-}
if [ -z "$root" ]; then
	# Same directory, resolved from our own location, for manual/test runs.
	root=$(cd -P -- "$(dirname -- "$0")/.." 2>/dev/null && pwd) || root=""
fi
manifest="$root/.claude-plugin/plugin.json"

# Match "version" as a whole JSON key: the quote before `version` keeps this off
# keys that merely end in the word (e.g. "schemaversion").
want=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" 2>/dev/null | head -n 1)
if [ -z "$want" ]; then
	say "cannot read the plugin version from $manifest; skipped"
	exit 0
fi
want=${want#v}

# ---- installed version: the managed binary itself --------------------------

# installed_version prints the version the managed binary reports ("ccb v0.4.3"
# -> "0.4.3"), empty if it is missing or unrunnable.
installed_version() {
	[ -x "$BIN" ] || return 0
	"$BIN" version 2>/dev/null | awk 'NR == 1 { sub(/^v/, "", $2); print $2 }'
}

have=$(installed_version)
[ "$have" = "$want" ] && exit 0 # fast path: nothing to do, no network, no output

# ---- one updater at a time -------------------------------------------------

mkdir -p "$CCB_DIR" 2>/dev/null || bail "cannot create $CCB_DIR"
if mkdir "$LOCK" 2>/dev/null; then
	locked=yes
elif [ -n "$(find "$LOCK" -maxdepth 0 -mmin +10 2>/dev/null)" ]; then
	# Older than 10 minutes: a previous run was killed before it could clean up.
	rmdir "$LOCK" 2>/dev/null
	mkdir "$LOCK" 2>/dev/null || exit 0
	locked=yes
	log "reclaimed a stale update lock"
else
	exit 0 # another session is on it; lose quietly
fi
trap 'cleanup; exit 0' EXIT
trap 'cleanup; exit 0' INT TERM HUP

# Re-read under the lock: the session that held it may have just installed this
# very version.
have=$(installed_version)
[ "$have" = "$want" ] && exit 0

# ---- download and verify ---------------------------------------------------

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) bail "unsupported OS: $os — see $BASE" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) bail "unsupported arch: $arch — see $BASE" ;;
esac

ASSET="ccb_${os}_${arch}"
URL="$BASE/download/v$want/$ASSET"
CK="$BASE/download/v$want/checksums.txt"

dl() {
	# dl <url> <output>
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		echo "need curl or wget to download $1" >&2
		return 1
	fi
}

# Stage inside the destination directory so the final move is a same-filesystem
# rename: the swap is atomic and never leaves a half-written binary in place.
mkdir -p "$BIN_DIR" 2>/dev/null || bail "cannot create $BIN_DIR"
tmpd=$(mktemp -d "$BIN_DIR/.update.XXXXXX") || bail "cannot create a temp dir in $BIN_DIR"

dl "$URL" "$tmpd/$ASSET" || bail "download failed: $URL"
dl "$CK" "$tmpd/checksums.txt" || bail "download failed: $CK"

sum=$(awk -v f="$ASSET" '$2 == f { print $1 }' "$tmpd/checksums.txt")
[ -n "$sum" ] || bail "no checksum for $ASSET in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmpd/$ASSET" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$tmpd/$ASSET" | awk '{ print $1 }')
else
	bail "need sha256sum or shasum to verify the download"
fi
[ "$sum" = "$got" ] || bail "checksum mismatch for $ASSET (v$want) — refusing to install"

chmod +x "$tmpd/$ASSET" || bail "cannot chmod +x the downloaded $ASSET"
mv -f "$tmpd/$ASSET" "$BIN" || bail "cannot install $BIN"

now=$(installed_version)
say "updated ${have:-none} -> ${now:-unknown} ($ASSET, wanted $want) at $BIN"
if [ "$now" != "$want" ]; then
	say "WARNING: v$want installed but the binary reports ${now:-nothing}; it will be re-downloaded every session until they agree"
fi

# ---- restart the daemon so it runs the new code ----------------------------

# The old `ccb watch` keeps running the replaced inode until it is restarted.
restart_daemon() {
	uid=$(id -u)
	if command -v launchctl >/dev/null 2>&1 &&
		launchctl print "gui/$uid/com.ccbroker.watch" >/dev/null 2>&1; then
		if launchctl kickstart -k "gui/$uid/com.ccbroker.watch" >/dev/null 2>&1; then
			log "restarted launchd job com.ccbroker.watch"
		else
			say "launchctl kickstart gui/$uid/com.ccbroker.watch failed; restart it yourself"
		fi
		return 0
	fi
	if command -v systemctl >/dev/null 2>&1 &&
		systemctl --user cat ccb-watch.service >/dev/null 2>&1; then
		if systemctl --user restart ccb-watch >/dev/null 2>&1; then
			log "restarted systemd user unit ccb-watch"
		else
			say "systemctl --user restart ccb-watch failed; restart it yourself"
		fi
		return 0
	fi
	# cron or hand-rolled supervision: stop the old process ourselves, then let
	# ensure-alive start a fresh one now instead of at the next cron tick.
	if command -v pkill >/dev/null 2>&1; then
		if pkill -u "$(id -u)" -f 'ccb watch' 2>/dev/null; then
			log "killed the running ccb watch"
			sleep 1 # let it release the pidfile lock before ensure-alive probes it
		fi
	else
		say "no pkill on this host; an already-running ccb watch keeps the old binary until it restarts"
	fi
	if "$BIN" ensure-alive >/dev/null 2>&1; then
		log "ran ccb ensure-alive"
	else
		say "ccb ensure-alive failed; start the watch daemon yourself"
	fi
}
restart_daemon

# ---- warn if the `ccb` on PATH is a different, unmanaged binary -------------

# resolve prints the symlink-resolved path of $1 (bounded against link loops).
resolve() {
	p=$1
	n=0
	while [ -L "$p" ] && [ "$n" -lt 20 ]; do
		d=$(dirname -- "$p")
		t=$(readlink -- "$p")
		case "$t" in
		/*) p=$t ;;
		*) p="$d/$t" ;;
		esac
		n=$((n + 1))
	done
	printf '%s\n' "$p"
}

check_path_ccb() {
	p=$(command -v ccb 2>/dev/null) || return 0
	[ -n "$p" ] && [ -e "$p" ] || return 0
	r=$(resolve "$p")
	[ "$r" != "$BIN" ] || return 0 # already converged
	# A shell shim that execs the managed binary (the plugin's bin/ccb) is fine.
	grep -q '\.config/ccbroker/bin/ccb' "$r" 2>/dev/null && return 0
	if [ "$p" = "$r" ]; then
		say "WARNING: the ccb on your PATH is $p, a separate copy — it stays on the old version. Converge it: ln -sf \"$BIN\" \"$p\""
	else
		say "WARNING: the ccb on your PATH is $p -> $r, not the managed binary $BIN — it stays on the old version. Converge it: ln -sf \"$BIN\" \"$p\""
	fi
}
check_path_ccb

cleanup
exit 0
