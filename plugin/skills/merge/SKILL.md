---
name: merge
description: Merging, promoting or tagging on a beekeeper desk: the gate, its exits 76 and 77, hold and release, rebase lines, merging and merged. Load before any of them.
---

# merge — one merge through the gate

- **The words.** `merging <n>` to the supervisor before and `merged <n> <version>` after every own
  merge; `tagging <v>` before a tag that rolls installations. The supervisor's `hold <repo>` stands
  until its `release <repo>`. A supervisor's own rule for its watch wins.
- **One merge command per PR, through the gate**: `devctl pr merge <owner/repo> <n>` for the owners
  under `merge.devctlOwners`, `devctl release promote <owner/repo>` likewise (the hook runs it
  under `beekeeper gate`). Act on its exit code, never around it: 76 queued, the merge waits on in
  its lane and its outcome wakes the worker, never run it again (park on the PR); 77 refused, its
  `beekeeper gate:` line goes to the supervisor, and no second merge follows.
- **Branch protection is read and proven with `devctl repo status <owner/repo>`** (its `protection`
  row names the rule and the required checks): the agent token answers `gh api
  repos/<owner>/<repo>/branches/<branch>/protection` with 403. A before/after proof around a merge
  is that command's row twice.
- **Nothing rolls onto an installation while one of its clusters upgrades.**
- **Before a non-mechanical change**, read the repo's open PRs and recent commits. A
  teammate's open PR that overlaps the task is a hard stop: tell the supervisor.
- **A personal account's repository**, outside the gate: the person's own `gh` once CI is green,
  never a GitHub App, `gh pr merge <n> --repo <owner/repo> --squash --subject "<PR title> (#<n>)"`
  (else `gh` takes a one-commit PR's message).
- **A release candidate is proven live and promoted as routine**; the person is never asked.
- **Commits.** Conventional subjects of at most 72 characters; no major version bumps.
- **A rebase-merge line** (`--rebase`) never merges its base into a branch, which hides a conflict:
  a PR only behind its base the gate rebases at its turn; one that conflicts is replaced at once
  by a PR from a fresh branch of the base.
