---
description: Show quota status for all broker-managed Claude accounts
---

Run `ccb status` with the Bash tool and show the user the output as-is (it is
a pre-formatted table: per-account 5h/7d utilization bars plus model-scoped
weekly limits). The `*` marks the active account. If an account is marked
DEAD, tell the user it needs a re-login and a re-import into the broker.
