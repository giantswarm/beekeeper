---
name: merge
description: "Loop-owned merges, promotions and tags through the gate."
---

# merge — one merge through the gate

- **Loop owns every merge.** Workers push, park at once; loop runs CI, merges after review and fixes; never ask the person. A changed head needs its exact-head go.
- **Report** `merging <n>` before, `merged <n> <version>` after; `tagging <v>` before rolling. Supervisor `hold <repo>` stands until `release <repo>`; watch rules win.
- **One command per PR:** `devctl pr merge <owner/repo> <n>` for `merge.devctlOwners` (the only path; rebase line: `--rebase`); promotion: `devctl release promote <owner/repo> --version <proven RC>`. Never bypass exits: 76 queued, park from worktree (below), outcome wakes you, never repeat; 77 refused, report its line, no second merge; 74 unread, repeat once GitHub answers; 3 still "behind": report the repository protection requirement; its owner decides whether to change it. Never update the branch to retry.
- **Personal loop:** after the go, `gh pr merge <n> --repo <owner/repo> --squash --match-head-commit <reviewed-head-sha> --subject "<PR title> (#<n>)"`.
- **Protection:** `gh api repos/<owner/repo>/rules/branches/main`; no `…/protection`.
- **No rollout during cluster upgrades.**
- **Overlap:** read PRs/commits; name it in your PR or build on the other. If blocked: `board block <yours> --by <other>`, `agents park --on <other-pr>`. No coordination wait.
- **RCs:** prove live; worker-rules govern promotion.
- **Commits:** Conventional, ≤72 characters; no major bumps.
- **Behind:** merge the PR as it stands when its head CI is green and the merge-result check against the current base passes. Never use `--update-branch`, rebase, or merge the base into a branch just to catch up. Conflicts still require a repair and green CI.
- **Desk review** immediately after push, one call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. Other dirs or unpushed heads record plain waits; re-run there.
