# ccbroker plugin

Claude account switching, quota status and auto-rotation policy for Claude Code,
backed by [ccbroker](https://github.com/Dev-Jahn/ccbroker) — a self-hosted broker
that centrally refreshes your Claude Code OAuth credentials and keeps quota
snapshots for every account. This plugin is a thin front-end: each command shells
out to the `ccb` client, so the broker does the actual credential and rotation
work.

## Commands

- `/ccb-status` — show the quota table (5h / 7d / per-model weekly) for every
  account your broker token can read; `*` marks the active one.
- `/ccb-use <name>` — switch the active account to `<name>`.
- `/ccb-auto` — keep the active account while it is under the threshold, else
  switch to the least-utilized live account per the configured rotation policy.
- `/ccb-policy [manual|account|all]` — show or set the auto-rotation policy.
- `/ccbroker:statusline [on|off]` — render the multi-account usage line, and
  turn it on or off in your Claude Code statusline.

## SessionStart hooks

- `scripts/ensure-ccb.sh` — keeps the `ccb` binary at **this plugin's version**
  (see below).
- `ccb sync` — so every new Claude Code session starts with a freshly refreshed
  token and an up-to-date quota cache. It also offers any local `/login`
  credential to the broker (the account on-ramp) before writing back the active
  account's token.

## The plugin version is the binary version

This plugin's version names the ccbroker release whose `ccb` belongs with it, so
updating the plugin updates the binary on every machine.

`ensure-ccb.sh` compares that version with what `~/.config/ccbroker/bin/ccb`
reports at each session start. Equal (the normal case): it exits immediately, no
network, no output. Different: it downloads the matching release asset, verifies
it against the release's `checksums.txt`, swaps it into
`~/.config/ccbroker/bin/ccb` atomically, and restarts the watch daemon (launchd,
systemd-user, or kill + `ccb ensure-alive`). Actions are logged to
`~/.config/ccbroker/selfupdate.log`. A failure never blocks the session: it warns
on stderr and exits 0, leaving the working binary in place.

`~/.config/ccbroker/bin/ccb` is the one real file; whatever `ccb` is on your PATH
should be a symlink to it (`install.sh` sets that up). If it is a separate copy
instead, the updater prints the `ln -sf` command that converges them.

`bin/ccb` in this plugin is a shim that execs the managed binary. A plugin's
`bin/` is on PATH for Bash tool calls only — not for hooks, the statusline or MCP
servers — so it is a convenience there, never what the daemon relies on.

## Requirements

A configured `agent.json` (`ccb setup`), and a broker to talk to. First-run:

```sh
curl -fsSL https://raw.githubusercontent.com/Dev-Jahn/ccbroker/main/install.sh | sh
ccb setup
```

See the
[ccbroker install instructions](https://github.com/Dev-Jahn/ccbroker#install).
