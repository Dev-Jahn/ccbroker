---
description: Turn the ccbroker multi-account statusline on or off
argument-hint: on|off
---

If `$ARGUMENTS` is `on` or `off`, run `ccb statusline $ARGUMENTS --settings ~/.claude/settings.json` with the Bash tool and relay its output. If it errors with a manual-integration message, show the user the printed marker block and where to put it.

If `$ARGUMENTS` is anything else (or empty), run `ccb statusline --all` and show the rendered line, then explain: `on` installs or idempotently updates the per-account usage line (all accounts, 5h/7d/per-model weekly, active marked ⛁) into the Claude Code statusline — either as the statusLine command itself or as a marker block appended to an existing statusline script; `off` removes exactly that line/block. Changes take effect on the next statusline refresh; no restart needed.
