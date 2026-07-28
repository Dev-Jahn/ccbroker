---
description: Switch the active Claude account (broker-managed)
argument-hint: <account-name>
---

Run `ccb use $ARGUMENTS` with the Bash tool.

- On success, state which account is now active — nothing more.
- If no argument was given, run `ccb status` first and ask which account to
  switch to.

Report only what the command did. The switch is live as soon as the command
returns, so never add commentary about it applying to new sessions, needing a
restart or a `/login`, or an in-memory token lingering.
