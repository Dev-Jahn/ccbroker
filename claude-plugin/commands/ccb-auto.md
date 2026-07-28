---
description: Auto-switch to the least-utilized Claude account
---

Run `ccb auto` with the Bash tool. It keeps the current account if it is under
the utilization threshold, otherwise switches to the least-utilized live
account, measured by the configured rotation policy (see /ccb-policy).

Report only what happened (kept vs switched, and to which account). A switch is
live as soon as the command returns, so never add commentary about it applying
to new sessions or needing a restart.
