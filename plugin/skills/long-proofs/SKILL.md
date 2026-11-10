---
name: long-proofs
description: Run long proofs, including full browser suites, beyond a Bash background job's lifetime. Load before launching a long proof.
---

# long-proofs — durable runs and completion wakes

A Bash background job is capped at 10 minutes, including the `beekeeper run`
slot wait. A full browser suite runs as a `systemd-run --user` unit, not a Bash
background job. Keep logs and the exit result in the task's scratch directory.

The user service manager does not inherit the launching shell's session
identity. Pass the identity with `--setenv=NAME` (without a value): systemd-run
copies that variable from its own environment. This lets `beekeeper secret copy`
inside the proof run as the starting session without `--as` or extra secret
flags. Forward only the identity variables below, never the whole environment
or vault credentials; do not use `systemctl --user import-environment`.

For example, run this in Bash with a unique unit name and an absolute working
directory. The allowlist covers Claude Code, Codex, agy, omp and managed workers;
only variables set in the launching shell are passed:

```bash
proof_identity=()
for name in CLAUDECODE CLAUDE_CODE_SESSION_ID CLAUDE_PID \
  CLAUDE_CODE_HOST_SESSION_ID CLAUDE_CODE_SESSION_NAME CODEX_THREAD_ID \
  BEEKEEPER_CODEX_AGENT BEEKEEPER_AGY_AGENT BEEKEEPER_OMP_AGENT \
  BEEKEEPER_AGENT_NAME BEEKEEPER_EXECUTION_ID BEEKEEPER_EXECUTION_COORDINATOR \
  BEEKEEPER_EXECUTION_HOME; do
  if [[ -v $name ]]; then
    proof_identity+=("--setenv=$name")
  fi
done
systemd-run --user "${proof_identity[@]}" --unit=<proof-unit> --property=RemainAfterExit=yes --working-directory=<worktree> /bin/bash -c '<proof command>'
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
