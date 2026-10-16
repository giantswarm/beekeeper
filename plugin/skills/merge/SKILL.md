---
name: merge
description: Merging, promoting or tagging on a beekeeper desk: the gate, its exits 76 and 77, protection, hold and release, rebase lines. Load before any.
---

# merge — one merge through the gate

- **The words.** `merging <n>` to the supervisor before and `merged <n> <version>` after every own merge; `tagging <v>` before a tag that rolls installations. The supervisor's `hold <repo>` stands
  until its `release <repo>`. A supervisor's watch rule wins.
- **One merge command per PR, through the gate**: `devctl pr merge <owner/repo> <n>` for the owners
  under `merge.devctlOwners`, `devctl release promote <owner/repo>` likewise (the hook runs it
  under `beekeeper gate`). Act on its exit code, never around it: 76 queued, the lane merges it on and
  its outcome wakes the worker (park on the PR, never run it again); 77 refused, its `beekeeper
  gate:` line goes to the supervisor, no second merge.
- **Protection** is read and proven with `devctl repo status <owner/repo>` (its `protection` row); `gh api …/protection`
  is 403.
- **Nothing rolls onto an installation while one of its clusters upgrades.**
- **Before a non-mechanical change**, read the repo's open PRs and recent commits: a
  teammate's overlapping open PR is a hard stop, told to the supervisor.
- **A personal account's repository**, outside the gate: the person's own `gh`, never a GitHub App,
  once CI is green: `gh pr merge <n> --repo <owner/repo> --squash --subject "<PR title> (#<n>)"`.
- **A release candidate is proven live and promoted as routine**; the person is never asked.
- **Commits.** Conventional subjects of at most 72 characters; no major version bumps.
- **A rebase-merge line** (`--rebase`) never merges its base into a branch: a PR only
  behind its base the gate rebases at its turn; one that conflicts is replaced by a PR from a
  fresh branch of the base.
