---
name: long-proofs
description: Run long proofs, including full browser suites, beyond a Bash background job's lifetime. Load before launching a long proof.
---

# long-proofs — durable runs and completion wakes

A Bash background job is capped at 10 minutes, including the `beekeeper run`
slot wait. A full browser suite runs as a `systemd-run --user` unit, not a Bash
background job. Keep logs and the exit result in the task's scratch directory.

For example, use a unique unit name and an absolute working directory:

```sh
systemd-run --user --unit=<proof-unit> --property=RemainAfterExit=yes --working-directory=<worktree> /bin/bash -c '<proof command>'
beekeeper timer add 1m "proof finished: inspect result" --probe 'case "$(systemctl --user show <proof-unit>.service --property=SubState --value)" in exited|failed) exit 0;; *) exit 1;; esac' --wake "<agent>" --until <duration>
```

Set the probe timer before parking; choose its duration for the suite and any
slot wait. Both success and failure must wake the worker. On resume, inspect
`systemctl --user show <proof-unit>.service --property=Result --property=ExecMainStatus`
and the saved logs: completion alone is not a passing proof. Keep `RemainAfterExit=yes` so a successful unit and its result survive until
inspection; do not use `--collect`.

An agentlab proof only waits: never under `beekeeper run`, whose build slot it
would hold. For other proofs, keep any required gate inside the unit's command.
Stop a unit with `systemctl --user stop <proof-unit>.service`.
