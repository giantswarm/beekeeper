---
name: park
description: Ending a turn with the task open on a beekeeper desk: parking on a person, a merge, a release or a probe timer. Load before such a turn ends.
---

# park — ending a turn with the task open

- **A worker that can go no further** until an answer, a merge ahead of it in the lane or a release
  comes parks and ends its turn, never sleeps on it: `beekeeper agents park --on <#note|owner/repo#n>
  "<what>"`. The inbox resume carries what settled it. A hold refusal (77) parks on that hold,
  `--on hold:<target>` as the refusal names it, and runs the same merge once the resume says it lifted.
- **Desk review**, immediately after push: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> --post-merge "<remaining work>" "desk review"`. Matching published HEAD requests review; otherwise re-park there. The loop merges. Omit `--post-merge` only if merge ends the task without a wake.
- **SSH CI:** `beekeeper agents park --checks --on <owner/repo#n> "CI"`; coordinator wakes on green/red; then fix or park for review.
- **Other waits:** `beekeeper timer add <time> "<what>" --when "<condition>"` or `--probe "<command>"`, with `--wake "<agent>" --until <duration>`; then park. CI: `pr-checks <owner/repo#n>`.
- **Overlap:** name it in your PR, build on the other, or park `--on <owner/repo#n>` for merge. No coordination wait.
- **Park only on external waits** or a person's decision. Report problems; continue work that can move.
- **A park on a person is news.** Tell the supervisor in one line at once for an answer,
  review, reply or note. The watch says PARKED ON A PERSON; the guide tells the person.
- **Before the turn ends**, `beekeeper sessions serve "<name>" <owner/repo#n> --waits "<what>"`
  carries the new wait, and nothing is left
  running. A park is not the report: `agents idle --done --report` comes when the task is finished.
