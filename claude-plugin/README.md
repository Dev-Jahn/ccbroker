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
- `/ccb-use <name>` — switch the active account to `<name>` (takes effect on new
  Claude Code sessions).
- `/ccb-auto` — keep the active account while it is under the threshold, else
  switch to the least-utilized live account per the configured rotation policy.
- `/ccb-policy [manual|account|all]` — show or set the auto-rotation policy.
- `/ccbroker:statusline [on|off]` — render the multi-account usage line, and
  turn it on or off in your Claude Code statusline.

## SessionStart hook

The plugin registers a `SessionStart` hook that runs `ccb pull`, so every new
Claude Code session starts with a freshly refreshed token and an up-to-date quota
cache.

## Requirements

Requires the `ccb` binary on your `PATH` and a configured `agent.json`. Install
the client and run the first-run wizard from the
[ccbroker install instructions](https://github.com/Dev-Jahn/ccbroker#install):

```sh
curl -fsSL https://raw.githubusercontent.com/Dev-Jahn/ccbroker/main/install.sh | sh
ccb setup
```
