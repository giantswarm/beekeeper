---
name: merge
description: Merging, promoting or tagging on a beekeeper desk: the gate, its exits 76 and 77, protection, hold and release, rebase lines. Load before any.
---

# merge — one merge through the gate

- **The words.** `merging <n>` to the supervisor before and `merged <n> <version>` after every own merge; `tagging <v>` before a tag that rolls installations. The supervisor's `hold <repo>` stands
  until its `release <repo>`. A supervisor's watch rule wins.
- **One merge command per PR, through the gate**: `devctl pr merge <owner/repo> <n> --update-branch`
  (a rebase line: `--rebase --update-branch`) for `merge.devctlOwners`, `devctl release promote
  <owner/repo> --version <proven RC>` likewise. Act on its exit code, never around it: 76 queued,
  its outcome wakes the worker (park on the PR, never run it again); 77 refused, its line to the
  supervisor, no second merge; 74 unread: the same merge once GitHub answers; 3 still "behind":
  as below, then the same merge again.
- **Protection** is read with `devctl repo status <owner/repo>`; `gh api …/protection` is 403.
- **Nothing rolls onto an installation while one of its clusters upgrades.**
- **Before a non-mechanical change**, read the repo's open PRs and recent commits: a
  teammate's overlapping one is a hard stop, told to the supervisor.
- **A personal account's repository**: the person's own `gh` once CI is green: `gh pr merge <n> --repo <owner/repo> --squash --subject "<PR title> (#<n>)"`.
- **A release candidate is proven live and promoted as routine**; the person is never asked.
- **Commits.** Conventional subjects, at most 72 characters; no major version bumps.
- **Behind**: a behind branch merges its base and pushes, never on a rebase line: the gate
  rebases it at its turn. There, a conflict or merge commit (77) takes a PR from a fresh
  branch of the base.
