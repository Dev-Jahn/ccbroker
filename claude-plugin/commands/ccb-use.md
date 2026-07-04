---
description: Switch the active Claude account (broker-managed)
argument-hint: <account-name>
---

Run `ccb use $ARGUMENTS` with the Bash tool.

- On success, confirm which account is now active and remind the user that the
  switch applies to NEW Claude Code sessions — the current session keeps its
  in-memory token until it ends.
- If no argument was given, run `ccb status` first and ask which account to
  switch to.
