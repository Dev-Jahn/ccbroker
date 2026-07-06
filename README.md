# ccbroker

Self-hosted broker that centrally refreshes Claude Code OAuth credentials for
all your machines, with quota-aware multi-account rotation.

[![CI](https://github.com/Dev-Jahn/ccbroker/actions/workflows/ci.yml/badge.svg)](https://github.com/Dev-Jahn/ccbroker/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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
                 │   ├─ refresh manager   single-flight, refresh ~1h early     │
                 │   │     POST https://api.anthropic.com/v1/oauth/token       │
                 │   ├─ credential API   :8787   Bearer token + per-cred scope │
                 │   └─ admin API        127.0.0.1:8788   X-Admin-Token        │
                 └───────────────────────────────┬────────────────────────────┘
                          (private tunnel, e.g. Tailscale / WireGuard)
              ┌───────────────────────────────────┼───────────────────────────┐
        ccb                                  ccb                          ccb
        (macOS: Keychain)                    (Linux: file)                (Linux: file)
        writes "Claude Code-credentials"     ~/.claude/.credentials.json  ~/.claude-work/...
```

* **`ccbrokerd`** — the daemon. Owns the credentials, refreshes them before
  expiry (single-flight per credential, exponential-ish backoff on transient
  failures, marks a credential dead on `invalid_grant`), and serves them over a
  bearer-authenticated, scope-limited API. A separate localhost-only admin API
  imports / lists / deletes / force-refreshes credentials.
* **`ccb`** — the client. Pulls named credentials on an interval and
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

## Install

**Client** (`ccb`) — macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/Dev-Jahn/ccbroker/main/install.sh | sh
ccb setup
```

`install.sh` downloads the release binary for your OS/arch, verifies it against
`checksums.txt`, and installs it to `~/.local/bin` (override with
`CCB_INSTALL_DIR`); `ccb setup` then walks you through the client config.

**Broker** (`ccbrokerd`) — Linux with systemd, run as root:

```sh
curl -fsSL https://raw.githubusercontent.com/Dev-Jahn/ccbroker/main/install-server.sh | sudo sh
```

`install-server.sh` installs `ccbrokerd`, generates a master key, an admin token
and a first client token, writes `/etc/ccbroker/config.json`, installs and starts
the systemd service, and prints the tokens once.

**Manual.** Grab a prebuilt binary for your platform from the
[releases page](https://github.com/Dev-Jahn/ccbroker/releases) (assets are named
`ccb_<os>_<arch>` / `ccbrokerd_linux_<arch>`, with a `checksums.txt`), or build
the client with Go:

```sh
go install github.com/Dev-Jahn/ccbroker/cmd/ccb@latest
```

## Quickstart

1. **Bring the broker up** on a server you control — the server one-liner above
   sets up `ccbrokerd`, a config and a systemd unit, and prints an admin token
   and a client token.
2. **Import a credential** from an existing Claude login (see
   [Import a credential](#import-a-credential)).
3. **On each machine**, install the client and run `ccb setup` — it asks for the
   broker URL and client token, writes `agent.json`, and offers to install a
   launchd/systemd job that keeps the credential fresh.
4. **Done.** Claude Code on that machine now reads a token the broker refreshes;
   use `ccb status` / `ccb use` / `ccb policy` to manage accounts.

## Run the broker

The [server one-liner](#install) (`install-server.sh`) does all of this for you.
To set the broker up by hand:

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

## Run the client (`ccb`)

```sh
ccb setup         # interactive first-run wizard: writes agent.json, installs the watch daemon
ccb sync          # one-shot sync (offer local /login, adopt, write active) — `pull` is a kept alias
ccb watch         # foreground daemon: long-poll the broker and sync on every rotation
ccb ensure-alive  # start `ccb watch` if it is not running (cron fallback)
ccb run           # loop on intervalSec
ccb use work      # switch the "@active" account and sync
ccb auto          # switch to the least-utilized account and sync
ccb status        # quota table for all accounts in scope
ccb statusline    # one-line summary for a Claude Code statusLine
ccb policy all    # show or set the auto-rotation policy (manual|account|all)
ccb version       # print the ccb version
```

See `examples/agent.example.json`. Targets:

* `{"type":"file","path":"~/.claude/.credentials.json"}` — Linux and anywhere
  Claude Code reads the file.
* `{"type":"keychain"}` — macOS; updates the `Claude Code-credentials` Keychain
  item, reusing the account of the existing item.

Optional `proxyUrl` reaches the broker through an explicit `http(s)://`,
`socks5://` or `socks5h://` proxy — e.g. `"proxyUrl": "socks5://localhost:1055"`
on a host whose tailscaled runs with `--tun=userspace-networking`, where tailnet
IPs are only reachable through its SOCKS5 server. As of v0.3.3, when `proxyUrl`
is empty the agent honors the standard proxy environment variables
(`HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`/`NO_PROXY`) — earlier versions ignored
them; if a global proxy is set, `NO_PROXY` can exempt the broker host.

## Account switching with a single config dir

Instead of one config dir per account (the CCS approach), keep the one
`~/.claude` every machine already has and swap which broker credential fills
it. A target whose `cred` is the literal `"@active"` follows the account named
in `activeFile` (default `~/.config/ccbroker/active`), which
`ccb use <name>` writes before syncing immediately:

```json
"targets": [ { "cred": "@active", "type": "file", "path": "~/.claude/.credentials.json" } ]
```

```sh
ccb use personal   # ~/.claude now authenticates as "personal"
ccb use work       # ...now as "work"
```

The periodic `run` loop keeps whatever is currently active fresh, and every
refresh still happens only on the broker. A Claude Code session already running
under the old account holds that account's token in memory, so it keeps using it
until the token expires (within ~8h); at that point the session may briefly show
"Not logged in", and resuming adopts the newly active account from disk
automatically — no `/login` needed. On machines running autonomous work, prefer
switching between sessions rather than during one so the switch never interrupts
a live run.

## Rotation policies

The broker polls `GET api.anthropic.com/api/oauth/usage` for every credential
(the endpoint reports 5-hour / 7-day / per-model-weekly utilization **without
consuming message quota**) and serves the snapshots at `/v1/usage`. `ccb status`
renders them; `ccb auto`, and `pull`/`run` when a rotation policy is active, use
them to pick the active account.

The **rotation policy** decides which quota windows can trigger an auto-switch:

| Policy | Switches the active account when… |
|--------|-----------------------------------|
| `manual` | never — you switch yourself with `ccb use <name>` |
| `account` | the account-wide 5-hour **or** 7-day window reaches `autoThreshold` (default 0.95) |
| `all` | any of the above **or** any per-model weekly bucket reaches the threshold |

**The per-model trap.** The account-wide 5h/7d windows can look healthy while a
single model's weekly bucket is already exhausted — top-tier models often carry
their own weekly limit. `account` **ignores model buckets by design**: a spent
per-model bucket does not block your other models, so rotating away from an
otherwise-fine account would waste its remaining capacity. Choose `all` only if
your workflow depends on one specific model and you would rather switch accounts
the moment that model's weekly bucket runs out.

Change the policy any time:

```sh
ccb policy            # show the effective policy and where it came from
ccb policy all        # set it (manual | account | all)
```

or run `/ccb-policy` in the Claude Code plugin, or edit `autoPolicy` in
`agent.json`. The legacy `"auto": true` flag still works and is equivalent to
`account`.

## Claude Code statusline

`ccb statusline` prints a one-line summary of the **active** account from the
cached snapshot (no network in the hot path):

```
personal 5h:16% 7d:62%
```

`ccb statusline --all` renders **every** account your token can read on one
line — 5h / 7d and each per-model weekly bucket, a dim `↻` countdown to each
account's next 7-day reset, the active account marked `⛁`, dead accounts marked
`✗`, each utilization colored by how full it is and the whole line suffixed
` ~stale` when the cache is old (shown here without ANSI color):

```
personal 5h:12% 7d:40% F:71% ↻2d3h │ ⛁ work 5h:3% 7d:22% F:9% ↻6d18h
```

Turn that full line on or off as your Claude Code statusline. Both are
idempotent — running either twice leaves the file byte-for-byte unchanged:

```sh
ccb statusline on                                   # ~/.claude/settings.json
ccb statusline on  --settings ~/.claude-work/settings.json
ccb statusline off                                  # remove it again
```

`on` writes `ccb statusline --all` as the statusLine of a settings file that
has none yet; if a statusLine already exists it instead appends a ccbroker
marker block to the statusline script that command points at (preserving the
script's mode), leaving your own statusline intact. `off` removes exactly
whatever `on` added. The legacy `ccb statusline --install` still works — it
writes the statusLine into `~/.claude/settings.json` and refuses to overwrite
an existing one:

```sh
ccb statusline --install               # writes statusLine into ~/.claude/settings.json
ccb statusline --install --settings ~/.claude-work/settings.json
```

## Claude Code plugin

`claude-plugin/` is a minimal Claude Code plugin exposing `/ccb-status`,
`/ccb-use <name>`, `/ccb-auto`, `/ccb-policy [manual|account|all]` and
`/ccbroker:statusline [on|off]` as slash commands plus a SessionStart hook that
runs `ccb sync` (fresh token + fresh quota cache at session start). It requires
`ccb` on PATH. Claude Code does not render statuslines from a plugin, so
`/ccbroker:statusline` just shells out to `ccb statusline on|off` to wire the
line into your `settings.json`.

Install it from the marketplace:

```
/plugin marketplace add Dev-Jahn/jahns-cc-marketplace
/plugin install ccbroker
```

### Or alongside CCS (profile switching)

If you prefer CCS-style separate profiles, the broker stays format-agnostic:
map credential names to each profile's `CLAUDE_CONFIG_DIR`:

```json
"targets": [
  { "cred": "personal", "type": "file", "path": "~/.claude/.credentials.json"  },
  { "cred": "work",     "type": "file", "path": "~/.claude-work/.credentials.json" }
]
```

CCS keeps switching profiles; the agent keeps each profile's token fresh.

## Public exposure behind a reverse proxy (mTLS)

To reach the broker from machines that can't share a tunnel (e.g. a tagged
Tailscale host), terminate TLS on a reverse proxy at your own domain and require
a client certificate there — the proxy rejects anyone without a cert before the
request ever reaches the broker, and the app-layer bearer + scope still apply.
Because TLS terminates on your own proxy, no third party sees the credential
bodies. The agent presents its cert via `clientCertPath` / `clientKeyPath`:

```json
{ "brokerUrl": "https://ccbroker.example.com",
  "token": "…", "clientCertPath": "~/.config/ccbroker/pki/host.crt",
  "clientKeyPath": "~/.config/ccbroker/pki/host.key", "targets": [ … ] }
```

nginx (e.g. via Nginx Proxy Manager's advanced config), trusting your client CA:

```nginx
ssl_client_certificate /data/custom_ssl/ccbroker-client-ca.pem;
ssl_verify_client on;
```

Keep the admin API off the proxy — it stays localhost-only on the broker host.

## Security model

* **Private by default.** Meant to run behind a private tunnel
  (Tailscale/WireGuard); the credential API is then only as reachable as your
  tunnel. If you must expose it publicly, put it behind a reverse proxy that
  enforces **mTLS** so the bearer token is never the only thing between the
  internet and your credentials (see the reverse-proxy section above).
* **Encrypted at rest.** The store is AES-256-GCM with a 32-byte master key kept
  in a separate `0600` file.
* **Per-client bearer tokens, hashed at rest.** The config stores only
  `sha256(token)`; comparison is constant-time. Each client has **scopes**
  limiting which credential names it may read. Every access is written to an
  audit log.
* **Admin API is localhost-only** and separately authenticated.
* **Canonical sources.** The only official sources for `ccbroker` binaries are
  this repository and its
  [GitHub Releases](https://github.com/Dev-Jahn/ccbroker/releases) (which
  `install.sh` / `install-server.sh` verify against `checksums.txt`). Do not run
  binaries for this tool from anywhere else.

## API

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| GET | `/healthz` | none | liveness |
| GET | `/v1/credentials/{name}` | `Authorization: Bearer <token>` + scope | current credential for `name` (refresh token stripped) + `{gen, account}` envelope; `?probe=1` adds a fresh `probeLive`; `?sinceGen=G&waitSec=W` long-polls (`304` on timeout) |
| POST | `/v1/creds/offer` | `Authorization: Bearer <token>` | offer a local `/login` credential for account-routed adoption |
| GET | `/v1/usage` | `Authorization: Bearer <token>` | quota snapshots for all creds in scope (no tokens) |
| PUT | `/admin/creds/{name}` | `X-Admin-Token` (localhost) | import/replace a credential (verifies live + records account) |
| GET | `/admin/creds` | `X-Admin-Token` (localhost) | list (redacted) |
| DELETE | `/admin/creds/{name}` | `X-Admin-Token` (localhost) | remove |
| POST | `/admin/creds/{name}/refresh` | `X-Admin-Token` (localhost) | force refresh now |

The credential endpoint strips the refresh token before serving (unless
`serveRefreshToken` is set during the rollout window — see
[Upgrading to v0.4.0](#upgrading-to-v040-refresh-token-quarantine)). It never
returns an already-expired token; it serves `409` for a `suspect`/`dead`
credential and `503` for an expired one, so the client keeps its last good copy.

## Upgrading to v0.4.0 (refresh-token quarantine)

v0.4.0 closes the multi-refresher logout class: two machines sharing one OAuth
lineage could each legally refresh (any copy whose token is expired-per-clock
refreshes per Claude Code's own rules), revoking the broker's generation and, on
a replay of a superseded refresh token, killing the lineage. The fix makes the
broker the **single writer** of the refresh chain.

**What changes**

* **Refresh-token quarantine (invariant I1).** Refresh tokens now live *only* in
  the broker store. The broker strips the `refreshToken` when it serves a
  credential, and the client strips it again on every write — so no client disk
  ever holds a refresh token in the steady state, and no client can refresh
  (Claude Code with a refresh-token-less credential simply never refreshes; it
  recovers when the broker updates the token). The one transient exception is a
  machine that just ran `/login`: it holds the new lineage's refresh token until
  `ccb sync` **offers** it upstream — and that token is never destroyed locally
  until the broker provably holds a live, same-account credential.
* **Account-routed offer/adopt.** `ccb sync` (and `ccb use`, and the SessionStart
  hook) offer any local `/login` credential to `POST /v1/creds/offer`. The broker
  verifies the token is live, routes it to the matching credential **by account**
  (not by name), checks an anti-rollback ring, and adopts it. A fresh `/login`
  therefore enters the system through the front door instead of clobbering a
  managed credential.
* **Health lifecycle.** An access token that returns a confirmed 401/403-revoked
  while still unexpired marks the credential `suspect`; the broker serves `409`
  (so it stops handing out a revoked token within ~5 min instead of ~3 h) and
  does **not** immediately replay the refresh token. Recovery is either an
  incoming `/login` offer or, after 10 min with no offer, a single last-resort
  reclaim refresh. A `dead` credential is re-probed every 30 min and self-heals
  on a 200.
* **`ccb sync`** is the new name for `ccb pull` (the old name still works). The
  overwrite gate never destroys a local refresh token on any failure path
  (broker down, old broker, rate limit, verify outage, clock skew): the worst
  case keeps the local credential and retries next cycle.

**Rollout order — do this exactly (matches design §5)**

1. Release and deploy the **broker** with `"serveRefreshToken": true` in
   `config.json`. This keeps full backward compatibility: pre-v0.4 clients still
   receive a refresh token and self-heal; nothing is quarantined yet.
2. Upgrade **all** clients to v0.4.0 and convert their schedulers to the watch
   daemon (`ccb setup` re-run, or install `ccb watch`). Verify the offer path
   with a real `/login`, and that the overwrite gate keeps the local credential
   on a rejected offer.
   * **N-7 warning:** during this window, do **not** run `/login` on a machine
     that has not yet been upgraded — its old one-way `pull` will still clobber a
     fresh login. Keep the window short.
3. Flip the broker to `"serveRefreshToken": false` and run `ccb sync` once
   everywhere. Fleet disks lose their refresh tokens and the quarantine is
   active.

**Watch daemon is required (M-6).** Quarantine means only the broker refreshes,
so every rotation must reach each machine promptly. `ccb setup` installs a
**watch daemon** by default (launchd `KeepAlive` on macOS, a systemd-user service
with `Restart=always` on Linux, or a `*/5 ensure-alive` cron wrapper elsewhere),
which long-polls the broker and syncs within seconds of a rotation. It **also**
installs a periodic `ccb sync` watchdog: if the daemon dies, any outage is
bounded to one sync interval per rotation (default 30 min) rather than being
open-ended. On a Linux box that should keep syncing while logged out, run
`sudo loginctl enable-linger $USER`.

**Unknown accounts.** If `ccb sync` finds a local credential for an account the
broker does not manage **at all**, it does not touch it — it keeps the local
credential and warns loudly: that account is a multi-refresher hazard sitting
unmanaged on disk. Either log out of it, or import it into the broker
(`PUT /admin/creds/{name}`) so the broker becomes its single refresher. Note the
v0.4.0 import verifies the token live and records the account before storing, so
import requires the profile endpoint to be reachable.

## Caveats

* **Terms of service.** This uses a subscription OAuth token outside the
  official Claude Code client. Intended for personal, self-hosted use with your
  own account. Understand your provider's terms before using it.
* **Offline windows.** The broker rotates a credential's refresh token
  `refreshSkewSec` before its access token expires, so every machine must pull
  the rotated token while its old access token is still valid. Keep each agent's
  `intervalSec` (and any cron cadence) well **under** the broker's
  `refreshSkewSec` (default 1 h; the default pairing is a 30 min pull vs a 1 h
  skew). A machine that stays offline past that window can fall back to a local
  refresh, rotating the broker's refresh token out from under it and forcing a
  re-login.
* **Master key.** If the key file is lost the store cannot be decrypted; the
  only cost is re-importing credentials (re-login), but back the key up
  somewhere safe.

## License

MIT — see [LICENSE](LICENSE).
