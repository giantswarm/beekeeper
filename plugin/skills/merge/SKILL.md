---
name: merge
description: "Loop-owned merges, promotions and tags through the gate."
---

# merge — one merge through the gate

- **Loop owns every merge.** Workers push and immediately park; the loop runs CI before review. The loop merges after review and fixes; never ask the person. A changed head needs its exact-head go.
- **Report** `merging <n>` before, `merged <n> <version>` after; `tagging <v>` before rolling. Supervisor `hold <repo>` stands until `release <repo>`; watch rules win.
- **One command per PR:** `devctl pr merge <owner/repo> <n> --update-branch` for `merge.devctlOwners` (the only path; rebase line: `--rebase --update-branch`); promotion: `devctl release promote <owner/repo> --version <proven RC>`. Never bypass exits: 76 queued, park from worktree (below), outcome wakes you, never repeat; 77 refused, report its line, no second merge; 74 unread, repeat once GitHub answers; 3 still "behind": as below, then the same merge again.
- **Personal loop:** after the go, `gh pr merge <n> --repo <owner/repo> --squash --match-head-commit <reviewed-head-sha> --subject "<PR title> (#<n>)"`.
- **Protection:** `gh api repos/<owner/repo>/rules/branches/main`; no `…/protection`.
- **No rollout during cluster upgrades.**
- **Overlap:** read PRs/commits; name it in your PR or build on the other. If blocked: `board block <yours> --by <other>`, `agents park --on <other-pr>`. No coordination wait.
- **RCs:** prove live; worker-rules govern promotion.
- **Commits:** Conventional, ≤72 characters; no major bumps.
- **Behind:** before approval a behind branch merges its base and pushes, never on a rebase line; then the merge does. A conflict or merge commit (77): a fresh branch and PR off base.
- **Desk review** immediately after push, one call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. Other dirs or unpushed heads record plain waits; re-run there.
