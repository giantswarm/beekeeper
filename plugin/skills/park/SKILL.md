---
name: park
description: Ending a turn with the task open on a beekeeper desk: parking on a person, a merge, a release or a probe timer. Load before such a turn ends.
---

# park — ending a turn with the task open

- **A worker that can go no further** until an answer, a merge ahead of it in the lane or a release
  comes parks and ends its turn, never sleeps on it: `beekeeper agents park --on <#note|owner/repo#n>
  "<what>"`. The inbox resume carries what settled it. A hold refusal (77) parks on that hold,
  `--on hold:<target>` as the refusal names it, and runs the same merge once the resume says it lifted.
- **Desk review**, after push/green CI: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> --post-merge "<remaining work>" "desk review"`. Matching published HEAD requests review; otherwise re-park there. The loop merges. Omit `--post-merge` only if merge ends the task without a wake.
- **External state** (rollout, registry, CI outside `devctl`): set a wake timer, then park: `beekeeper timer add <time> "<what>" --when "<condition>"` (CI: `pr-checks <owner/repo#n>`, green or red)
  or `--probe "<command>"`, with `--wake "<agent>" --until <duration>`. A rollout is
  checked once, when its timer falls due.
- **Park only on a wait outside the task's hands** or a decision a person must make, never on a
  problem: a problem is reported to the supervisor and every part of the task that still moves goes
  on.
- **A park on a person is news.** Tell the supervisor in one line at once for an answer,
  review, reply or note. The watch says PARKED ON A PERSON; the guide tells the person.
- **Before the turn ends**, `beekeeper sessions serve "<name>" <owner/repo#n> --waits "<what>"`
  carries the new wait, and nothing is left
  running. A park is not the report: `agents idle --done --report` comes when the task is finished.
