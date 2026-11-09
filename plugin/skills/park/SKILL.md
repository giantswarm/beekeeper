---
name: park
description: Ending an open task on a beekeeper desk. Load before parking.
---

# park — ending a turn with the task open

- **External waits:** `agents park --on <#note|owner/repo#n> "<wait>"`, then end the turn. Events go to a fresh, capacity-selected worker with the task, wait and event; never resume the old transcript. Hold refusal 77: park on the named `hold:<target>`; retry once lifted.
- **Desk review, immediately after push:** `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> --post-merge "<remaining work>" "desk review"`. Matching published HEAD requests review. The loop owns CI and merge. Omit `--post-merge` only when merge ends the task without a continuation.
- **SSH CI:** `agents park --checks --on <owner/repo#n> "CI"`. Green goes directly to review; red goes to a fresh worker.
- **Mechanical waits:** no CI or GitHub-budget wake timers; the loop retries its own work. Release/rollout conditions belong in timer probes. A known authorized next command uses `timer add --run "<command>"`; never invent a command from prose or use a model just to check readiness.
- **Reasoning after a condition:** `timer add <time> "<what>" --when "<condition>"` or `--probe "<command>"`, with `--wake "<worker>" --until <duration>`; then park.
- **Overlap:** name it in the PR, build on it, or park on its merge. No coordination wait. Report barriers and continue independent work; park only on external waits or a person's decision.
- **Person waits are news:** tell the supervisor in one line at once. The watch says PARKED ON A PERSON; the guide relays it.
- **Before ending:** `sessions serve "<name>" <owner/repo#n> --waits "<what>"`; leave nothing running. Park is not the final report: use `agents idle --done --report` only when finished.
