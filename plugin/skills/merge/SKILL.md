---
name: merge
description: Merging, promoting or tagging on a beekeeper desk: the gate, its exits 76 and 77, hold and release, rebase lines, merging and merged. Load before any of them.
---

# merge — one merge through the gate

- **The words.** `merging <n>` to the supervisor before and `merged <n> <version>` after every own
  merge; `tagging <v>` before a tag that rolls installations. The supervisor's `hold <repo>` stands
  until its `release <repo>`. A different rule the supervisor states for its watch wins.
- **One merge command per PR, through the gate**: `devctl pr merge <owner/repo> <n>` for the owners
  under `merge.devctlOwners`, `devctl release promote <owner/repo>` likewise (the hook runs it
  under `beekeeper gate`). Act on its exit code, never around it: 76 queued, the merge waits on in
  its lane and its outcome wakes the worker, never run it again (park on the PR); 77 refused, its
  `beekeeper gate:` line says why, which goes to the supervisor, and no second merge follows.
- **Nothing rolls onto an installation while one of its clusters upgrades.**
- **Before a non-mechanical change**, read the repository's open PRs and recent commits. A
  teammate's open PR that overlaps the task is a hard stop: tell the supervisor.
- **A personal account's repository**, outside the gate, is merged with the person's own `gh`
  login once CI is green, never through a GitHub App.
- **A release candidate is proven live and promoted as routine**; the person is never asked to
  promote or merge.
- **Commits.** Conventional subjects of at most 72 characters; no major version bumps.
- **A rebase-merge line** (merged with `--rebase`) never merges its base into a branch, which only
  hides a conflict until the merge: a PR there that conflicts with its base (the gate refuses it
  before it queues) is replaced at once by a new PR from a fresh branch of the base.
