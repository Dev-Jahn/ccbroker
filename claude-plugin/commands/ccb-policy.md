---
description: Show or set the ccbroker auto-rotation policy
argument-hint: [manual|account|all]
---

If `$ARGUMENTS` is empty, run `ccb policy` with the Bash tool and explain the
result: `manual` = never auto-switches, `account` = switches when the 5h/7d
account-wide windows hit the threshold, `all` = also switches when any
per-model weekly bucket (e.g. a top-tier model's weekly limit) hits it.

If an argument is given, validate it is one of manual|account|all, run
`ccb policy $ARGUMENTS`, confirm the change, and remind the user it takes
effect on the next `ccb pull`/`ccb run` cycle.
