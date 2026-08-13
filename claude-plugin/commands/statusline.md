---
description: Install or refresh the ccbroker multi-account statusline (or turn it off)
argument-hint: on|off
---

If `$ARGUMENTS` is `off`, run `ccb statusline off --settings ~/.claude/settings.json` with the Bash tool and relay its output.

Otherwise (`on`, empty, or anything else), run `ccb statusline on --settings ~/.claude/settings.json` with the Bash tool and relay its output. This is idempotent: it installs the per-account usage line (all accounts, 5h/7d/per-model weekly, active marked ⛁) as the Claude Code statusLine command, or appends it as a marker block to an existing statusline script, and re-running it just refreshes what is already there.

If the command errors with a manual-integration message, show the user the printed marker block and where to put it. Do not add commentary about when the change takes effect or about restarting anything.
