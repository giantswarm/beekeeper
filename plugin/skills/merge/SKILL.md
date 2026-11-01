---
name: merge
description: Load before merges/promotions/tags: the gate, exits, protection, holds and rebase lines.
---

# merge — one merge through the gate

- **Every merge, personal repositories too:** independent desk review, green CI, then the loop's exact-head go. A changed head needs a new go.
- **Report** `merging <n>` before, `merged <n> <version>` after; `tagging <v>` before rolling. Supervisor `hold <repo>` stands until `release <repo>`; watch rules win.
- **One command per PR:** `devctl pr merge <owner/repo> <n> --update-branch` for `merge.devctlOwners`, their only path: gh and curl merges are refused (rebase line: `--rebase --update-branch`); promotion: `devctl release promote <owner/repo> --version <proven RC>`. Never bypass exits: 76 queued, park from its worktree (below), outcome wakes you, never repeat; 77 refused, report its line, no second merge; 74 unread, repeat once GitHub answers; 3 still "behind": as below, then the same merge again.
- **Personal:** after the go, `gh pr merge <n> --repo <owner/repo> --squash --match-head-commit <reviewed-head-sha> --subject "<PR title> (#<n>)"`.
- **Protection:** `devctl repo status <owner/repo>`; `gh api …/protection` is 403.
- **No rollout while any cluster of the installation upgrades.**
- **Before non-mechanical changes:** read open PRs/recent commits; stop on overlap and tell the supervisor.
- **Release candidates:** prove live and promote routinely; never ask the person.
- **Commits:** Conventional subjects, at most 72 characters; no major bumps.
- **Behind:** a behind branch merges its base and pushes, never on a rebase line: the gate rebases at its turn. A conflict or merge commit (77) needs a fresh branch and PR from the base.
- **Desk review** after push and green CI, in one shell call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. Other directories or unpushed heads record plain waits; re-run there.
