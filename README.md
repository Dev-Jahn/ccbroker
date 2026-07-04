# cc-cred-broker

A small self-hosted broker that holds Claude Code (Anthropic subscription)
OAuth credentials **centrally**, refreshes each one in a **single place**, and
hands them out to your machines. This makes it safe to use one Claude login
across several computers without the refresh-token rotation logouts you
otherwise hit.

## The problem it solves

Claude Code's subscription OAuth uses short-lived access tokens (~8h) plus a
refresh token that **rotates on every refresh**: when a token is refreshed, the
old refresh token is invalidated. If two machines share the same credential,
whichever refreshes first wins and the others are forced to log in again.

Copying the credential file around does not fix this — there is always a race
between one machine refreshing and the copy propagating. The only robust fix is
to make **exactly one** component responsible for refreshing, and have every
machine read from it. That component is this broker.

## How it works

```
                 ┌──────────────────────── broker host (private) ─────────────┐
                 │  ccbrokerd                                                  │
                 │   ├─ encrypted store (AES-256-GCM)   one record per cred    │
                 │   ├─ refresh manager   single-flight, refresh ~10m early    │
                 │   │     POST https://api.anthropic.com/v1/oauth/token       │
                 │   ├─ credential API   :8787   Bearer token + per-cred scope │
                 │   └─ admin API        127.0.0.1:8788   X-Admin-Token        │
                 └───────────────────────────────┬────────────────────────────┘
                          (private tunnel, e.g. Tailscale / WireGuard)
              ┌───────────────────────────────────┼───────────────────────────┐
        ccbroker-agent                       ccbroker-agent               ccbroker-agent
        (macOS: Keychain)                    (Linux: file)                (Linux: file)
        writes "Claude Code-credentials"     ~/.claude/.credentials.json  ~/.claude-work/...
```

* **`ccbrokerd`** — the daemon. Owns the credentials, refreshes them before
  expiry (single-flight per credential, exponential-ish backoff on transient
  failures, marks a credential dead on `invalid_grant`), and serves them over a
  bearer-authenticated, scope-limited API. A separate localhost-only admin API
  imports / lists / deletes / force-refreshes credentials.
* **`ccbroker-agent`** — the client. Pulls named credentials on an interval and
  writes them to local destinations: a `.credentials.json` file (Linux) or the
  macOS Keychain item Claude Code reads (`Claude Code-credentials`). Because the
  agent keeps the local token fresh, Claude Code never needs to refresh it
  itself, so it never rotates the broker's refresh token.

### Why `api.anthropic.com`

The documented token endpoint `https://console.anthropic.com/v1/oauth/token`
sits behind a Cloudflare managed challenge that blocks non-browser clients.
`https://api.anthropic.com/v1/oauth/token` serves the same
`grant_type=refresh_token` exchange without the challenge, which is what the
broker uses.

## Security model

* **No public exposure.** Designed to run behind a private tunnel
  (Tailscale/WireGuard). The credential API is only as reachable as your tunnel.
* **Encrypted at rest.** The store is AES-256-GCM with a 32-byte master key kept
  in a separate `0600` file.
* **Per-client bearer tokens, hashed at rest.** The config stores only
  `sha256(token)`; comparison is constant-time. Each client has **scopes**
  limiting which credential names it may read. Every access is written to an
  audit log.
* **Admin API is localhost-only** and separately authenticated.

## Build

Pure standard library, no external Go dependencies.

```sh
go build -o ccbrokerd       ./cmd/ccbrokerd
go build -o ccbroker-agent  ./cmd/ccbroker-agent

# cross-compile an agent for another machine
GOOS=darwin GOARCH=arm64 go build -o ccbroker-agent.darwin-arm64 ./cmd/ccbroker-agent
GOOS=linux  GOARCH=amd64 go build -o ccbroker-agent.linux-amd64  ./cmd/ccbroker-agent
```

## Run the broker

```sh
# 1. master key
ccbrokerd genkey > /etc/ccbroker/master.key && chmod 600 /etc/ccbroker/master.key

# 2. a client token (give the token to the machine; store only its hash)
TOKEN=$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')
echo "client token: $TOKEN"
printf %s "$TOKEN" | ccbrokerd hashtoken     # -> put in config.json clients[].tokenSha256

# 3. config.json (see examples/config.example.json), then:
ccbrokerd serve -c /etc/ccbroker/config.json
```

See `deploy/setup.sh` for a full first-time setup and `deploy/ccbrokerd.service`
for a systemd unit.

### Import a credential

Seed the broker from an existing login (`~/.claude/.credentials.json`, or the
JSON stored in the macOS Keychain item `Claude Code-credentials`):

```sh
curl -sS -X PUT -H "X-Admin-Token: $ADMIN_TOKEN" \
  --data @credentials.json \
  http://127.0.0.1:8788/admin/creds/personal
```

The body may be a full `{"claudeAiOauth": {...}}` file or the bare oauth object;
a `refreshToken` is required. From then on the broker refreshes it.

## Run an agent

```sh
ccbroker-agent pull -c agent.json   # one-shot
ccbroker-agent run  -c agent.json   # loop on intervalSec
```

See `examples/agent.example.json`. Targets:

* `{"type":"file","path":"~/.claude/.credentials.json"}` — Linux and anywhere
  Claude Code reads the file.
* `{"type":"keychain"}` — macOS; updates the `Claude Code-credentials` Keychain
  item, reusing the account of the existing item.

## Using it alongside CCS (profile switching)

CCS switches profiles by pointing `CLAUDE_CONFIG_DIR` at different directories,
each with its own `.credentials.json`. The broker stays format-agnostic: give
each managed account a name, and map names to those config dirs in the agent:

```json
"targets": [
  { "cred": "personal", "type": "file", "path": "~/.claude/.credentials.json"  },
  { "cred": "work",     "type": "file", "path": "~/.claude-work/.credentials.json" }
]
```

CCS keeps switching profiles; the agent keeps each profile's token fresh.

## API

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/healthz` | none | liveness |
| GET | `/v1/credentials/{name}` | `Authorization: Bearer <token>` + scope | current `.credentials.json` for `name` |
| PUT | `/admin/creds/{name}` | `X-Admin-Token` (localhost) | import/replace a credential |
| GET | `/admin/creds` | `X-Admin-Token` (localhost) | list (redacted) |
| DELETE | `/admin/creds/{name}` | `X-Admin-Token` (localhost) | remove |
| POST | `/admin/creds/{name}/refresh` | `X-Admin-Token` (localhost) | force refresh now |

The credential endpoint never returns an already-expired token (a client that
received one would refresh it itself and rotate the broker's refresh token out
from under it); it returns `503` instead so the client keeps its last good copy.

## Caveats

* **Terms of service.** This uses a subscription OAuth token outside the
  official Claude Code client. Intended for personal, self-hosted use with your
  own account. Understand your provider's terms before using it.
* **Offline windows.** If a machine cannot reach the broker for longer than the
  access-token lifetime *and* Claude Code tries to refresh locally, it will
  rotate the broker's refresh token and cause a conflict. Keep `intervalSec`
  well under the token lifetime (default 30 min vs ~8 h).
* **Master key.** If the key file is lost the store cannot be decrypted; the
  only cost is re-importing credentials (re-login), but back the key up
  somewhere safe.
