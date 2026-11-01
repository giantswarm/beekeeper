---
name: park
description: Ending a turn with the task open on a beekeeper desk: parking on a person, a merge, a release or a probe timer. Load before such a turn ends.
---

# park — ending a turn with the task open

- **A worker that can go no further** until an answer, a merge ahead of it in the lane or a release
  comes parks and ends its turn, never sleeps on it: `beekeeper agents park --on <#note|owner/repo#n>
  "<what>"`. The resume (the supervisor's `beekeeper agents wake`, or the watch's, delivered through
  the inbox) carries what settled the wait. A merge the gate refused on a hold (exit 77) parks on that hold,
  `--on hold:<target>` as the refusal names it, and runs the same merge once the resume says it lifted.
- **Requesting desk review** requires the published worktree in the same shell call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. The command verifies that worktree's branch and HEAD against the PR. Running from a desk checkout or an unpushed head records a plain wait, with no reviewer, go or findings forwarded; re-run from the published worktree to request review.
- **A wait on an external state** (a rollout, a registry, a CI outside `devctl`) is a timer
  that wakes the worker, then the park: `beekeeper timer add <time> "<what>" --when "<condition>"`
  or `--probe "<command>"`, with `--wake "<agent>" --until <duration>`. A rollout is
  checked once, when its timer falls due.
- **Park only on a wait outside the task's hands** or a decision a person must make, never on a
  problem: a problem is reported to the supervisor and every part of the task that still moves goes
  on.
- **A park on a person is news.** A worker that parks on a person (the person's answer, a
  colleague's review or reply, a note) tells the supervisor in one line at once, and the watch says
  it too (PARKED ON A PERSON): the guide carries it to the person, who otherwise never learns the
  work waits on them.
- **Before the turn ends**, `beekeeper sessions serve "<name>" <owner/repo#n> --waits "<what>"`
  carries the new wait, and nothing is left
  running. A park is not the report: `agents idle --done --report` comes when the task is finished.
