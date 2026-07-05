#!/bin/sh
# install-server.sh — install the ccbroker daemon (ccbrokerd) as a systemd
# service. Linux + systemd only. Run as root:
#   curl -fsSL https://raw.githubusercontent.com/Dev-Jahn/ccbroker/main/install-server.sh | sudo sh
#
# Env override:
#   CCB_VERSION  release tag to install (default: latest)
set -eu

BASE="https://github.com/Dev-Jahn/ccbroker/releases"
BIN=/usr/local/bin/ccbrokerd

if [ "$(id -u)" != 0 ]; then
  echo "install-server.sh must run as root (use sudo)" >&2
  exit 1
fi

os=$(uname -s)
[ "$os" = Linux ] || { echo "the broker only runs on Linux with systemd (got $os) — see $BASE" >&2; exit 1; }

arch=$(uname -m)
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported arch: $arch — see $BASE" >&2; exit 1 ;;
esac

VERSION="${CCB_VERSION:-latest}"
ASSET="ccbrokerd_linux_${arch}"
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

install -m 0755 "$tmp/$ASSET" "$BIN"

# Update path: config already present -> just refresh the binary and restart.
if [ -f /etc/ccbroker/config.json ]; then
  systemctl restart ccbrokerd
  echo "updated $BIN and restarted ccbrokerd (existing config left untouched)"
  exit 0
fi

# First-time setup.
mkdir -p /etc/ccbroker /var/lib/ccbroker /var/log/ccbroker
umask 077
[ -f /etc/ccbroker/master.key ] || "$BIN" genkey > /etc/ccbroker/master.key
chmod 600 /etc/ccbroker/master.key

rnd() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
CLIENT_TOKEN=$(rnd)
ADMIN_TOKEN=$(rnd)
CLIENT_HASH=$(printf %s "$CLIENT_TOKEN" | "$BIN" hashtoken)

cat > /etc/ccbroker/config.json <<EOF
{
  "listen": ":8787",
  "adminListen": "127.0.0.1:8788",
  "adminToken": "$ADMIN_TOKEN",
  "storePath": "/var/lib/ccbroker/store.enc",
  "keyPath": "/etc/ccbroker/master.key",
  "auditLog": "/var/log/ccbroker/audit.log",
  "refreshSkewSec": 3600,
  "usagePollSec": 300,
  "clients": [
    {"name": "client1", "tokenSha256": "$CLIENT_HASH", "scopes": ["*"]}
  ]
}
EOF
chmod 600 /etc/ccbroker/config.json

# Print the tokens now, before the service is started: a first-start failure
# below exits non-zero and a rerun takes the update path, so this banner is the
# only chance to capture the plaintext CLIENT_TOKEN.
cat <<EOF

==================================================================
 STORE THESE NOW — the tokens are printed only once:

   ADMIN_TOKEN=$ADMIN_TOKEN
   CLIENT_TOKEN=$CLIENT_TOKEN
==================================================================
EOF

cat > /etc/systemd/system/ccbrokerd.service <<'EOF'
[Unit]
Description=Claude credential broker
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/ccbrokerd serve -c /etc/ccbroker/config.json
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/ccbroker /var/log/ccbroker
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now ccbrokerd
sleep 2
if ! systemctl is-active --quiet ccbrokerd; then
  echo "ccbrokerd failed to start:" >&2
  journalctl -u ccbrokerd --no-pager -n 40 >&2
  exit 1
fi

echo "--- health ---"
dl "http://127.0.0.1:8787/healthz" /dev/stdout || true
echo

cat <<EOF

Next steps:
  1. Import a Claude credential (from a machine already logged in):
       curl -sS -X PUT -H "X-Admin-Token: $ADMIN_TOKEN" \\
         --data @credentials.json \\
         http://127.0.0.1:8788/admin/creds/personal

  2. Add more client machines — generate a token, hash it, and append a
     clients[] entry to /etc/ccbroker/config.json:
       TOKEN=\$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \\n')
       printf %s "\$TOKEN" | $BIN hashtoken
       systemctl restart ccbrokerd

  3. On each client machine, install ccb and run:
       ccb setup
     (use the CLIENT_TOKEN above and this host's broker URL)
EOF
