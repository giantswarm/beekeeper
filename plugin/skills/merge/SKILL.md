---
name: merge
description: "Load before merges/promotions/tags: the gate, exits, protection, holds and rebase lines."
---

# merge — one merge through the gate

- **Loop owns merges, personal repos too.** Workers push, wait for green CI and park for desk review. The supervisor loop merges after that review and its fixes; nobody asks the person whether to merge. A changed head needs the loop's new exact-head go.
- **Report** `merging <n>` before, `merged <n> <version>` after; `tagging <v>` before rolling. Supervisor `hold <repo>` stands until `release <repo>`; watch rules win.
- **One command per PR:** `devctl pr merge <owner/repo> <n> --update-branch` for `merge.devctlOwners` (the only path; rebase line: `--rebase --update-branch`); promotion: `devctl release promote <owner/repo> --version <proven RC>`. Never bypass exits: 76 queued, park from worktree (below), outcome wakes you, never repeat; 77 refused, report its line, no second merge; 74 unread, repeat once GitHub answers; 3 still "behind": as below, then the same merge again.
- **Personal loop merge:** after the go, `gh pr merge <n> --repo <owner/repo> --squash --match-head-commit <reviewed-head-sha> --subject "<PR title> (#<n>)"`.
- **Protection:** `devctl repo status <owner/repo>`; `gh api …/protection` is 403.
- **No rollout during cluster upgrades.**
- **Overlap:** read open PRs/recent commits; never repair another PR's change in yours — `board block <yours> --by <other>`, `agents park --on <other-pr>`.
- **Release candidates:** prove live, promote routinely; never ask the person.
- **Commits:** Conventional subjects, ≤72 characters; no major bumps.
- **Behind:** before approval a behind branch merges its base and pushes, never on a rebase line; then the merge does. A conflict or merge commit (77): a fresh branch and PR off base.
- **Desk review** after push/green CI, one call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. Other dirs or unpushed heads record plain waits; re-run there.
