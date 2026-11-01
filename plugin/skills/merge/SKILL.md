---
name: merge
description: Load before merges, promotions or tags: the gate, its exits, protection, holds and rebase lines.
---

# merge — one merge through the gate

- **Report** `merging <n>` before and `merged <n> <version>` after to the supervisor; `tagging <v>` before a rolling tag. Its `hold <repo>` stands until `release <repo>`; watch rules win.
- **One merge command per PR, through the gate**: `devctl pr merge <owner/repo> <n> --update-branch`
  (a rebase line: `--rebase --update-branch`) for `merge.devctlOwners`, `devctl release promote
  <owner/repo> --version <proven RC>` likewise. Obey its exits: 76 queued, outcome wakes you: park from its worktree as below, never repeat; 77 refused: report its line, no second merge; 74 unread: repeat once GitHub answers; 3 still "behind": as below, then the same merge again.
- **Protection** is read with `devctl repo status <owner/repo>`; `gh api …/protection` is 403.
- **Nothing rolls onto an installation while one of its clusters upgrades.**
- **Before non-mechanical changes**, read open PRs and recent commits; report an overlapping
  teammate PR to the supervisor and stop.
- **A personal account's repository**: the person's own `gh` once CI is green: `gh pr merge <n> --repo <owner/repo> --squash --subject "<PR title> (#<n>)"`.
- **Prove a candidate live and promote routinely**; never ask the person.
- **Commits.** Conventional subjects, at most 72 characters; no major version bumps.
- **Behind**: a behind branch merges its base and pushes, never on a rebase line: the gate
  rebases it at its turn. There, a conflict or merge commit (77) takes a PR from a fresh
  branch of the base.
- **Desk review** after push and green CI, in one shell call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. Other directories or unpushed heads record plain waits; re-run there.
