---
name: long-proofs
description: Run long proofs, including full browser suites, beyond a Bash background job's lifetime. Load before launching a long proof.
---

# long-proofs — durable runs and completion wakes

Bash background jobs expire after 10 minutes, with slot waits. Use a
`systemd-run --user` unit; save logs and exit results in task scratch.

The service manager lacks session identity. Forward set identity variables via
name-only `--setenv` so `beekeeper secret copy` runs as the starting session
without extra flags; never import the full environment or credentials.

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
beekeeper timer add 1m "inspect proof" --probe 'case "$(systemctl --user show <proof-unit>.service --property=SubState --value)" in exited|failed) exit 0;; *) exit 1;; esac' --wake "<agent>" --until <duration>
```

Set the timer before parking; allow for the suite and slot wait. Both outcomes wake. Inspect saved logs and `systemctl --user show <proof-unit>.service
--property=Result --property=ExecMainStatus`: completion alone is not success.
Keep `RemainAfterExit=yes`; never `--collect`. Stop with
`systemctl --user stop <proof-unit>.service`.

An agentlab proof only waits: never under `beekeeper run`, whose slot it holds.
Keep other required gates inside the unit command.
