#!/usr/bin/env bash
set -euo pipefail
mkdir -p /etc/ccbroker /var/lib/ccbroker /var/log/ccbroker
umask 077
[ -f /etc/ccbroker/master.key ] || /usr/local/bin/ccbrokerd genkey > /etc/ccbroker/master.key
chmod 600 /etc/ccbroker/master.key

rnd() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
CLIENT_TOKEN=$(rnd)
ADMIN_TOKEN=$(rnd)
CLIENT_HASH=$(printf %s "$CLIENT_TOKEN" | /usr/local/bin/ccbrokerd hashtoken)

cat > /etc/ccbroker/config.json <<EOF
{
  "listen": ":8787",
  "adminListen": "127.0.0.1:8788",
  "adminToken": "$ADMIN_TOKEN",
  "storePath": "/var/lib/ccbroker/store.enc",
  "keyPath": "/etc/ccbroker/master.key",
  "auditLog": "/var/log/ccbroker/audit.log",
  "refreshSkewSec": 600,
  "clients": [
    {"name": "test", "tokenSha256": "$CLIENT_HASH", "scopes": ["*"]}
  ]
}
EOF
chmod 600 /etc/ccbroker/config.json

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
  echo "SERVICE FAILED:"; journalctl -u ccbrokerd --no-pager -n 40; exit 1
fi
echo "SERVICE_ACTIVE"
echo "CLIENT_TOKEN=$CLIENT_TOKEN"
echo "ADMIN_TOKEN=$ADMIN_TOKEN"
echo "--- health ---"; curl -sS http://127.0.0.1:8787/healthz
echo "--- audit tail ---"; tail -n 5 /var/log/ccbroker/audit.log 2>/dev/null || true
