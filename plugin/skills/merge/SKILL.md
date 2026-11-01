---
name: merge
description: Merging, promoting or tagging on a beekeeper desk: the gate, exits 76/77, protection, holds, releases and rebase lines. Load before any.
---

# merge — one merge through the gate

- **Every desk merge, personal repositories included:** independent desk review, green CI and the loop's recorded go for the exact head. CI alone is insufficient; a changed head needs a new go.
- **Tell the supervisor** `merging <n>` before, `merged <n> <version>` after; `tagging <v>` before a rolling tag. Its `hold <repo>` stands until `release <repo>`; its watch rule wins.
- **One command per PR:** `devctl pr merge <owner/repo> <n> --update-branch` for `merge.devctlOwners` (rebase line: `--rebase --update-branch`); promotion: `devctl release promote <owner/repo> --version <proven RC>`. Never bypass exits: 76 queued, park on the PR, its outcome wakes you, never repeat; 77 refused, report its line, no second merge; 74 unread, repeat once GitHub answers; 3 still "behind": as below, then the same merge again.
- **Personal repository:** after the go, `gh pr merge <n> --repo <owner/repo> --squash --match-head-commit <reviewed-head-sha> --subject "<PR title> (#<n>)"`.
- **Protection:** `devctl repo status <owner/repo>`; `gh api …/protection` is 403.
- **No rollout while any cluster of the installation upgrades.**
- **Before non-mechanical changes:** read open PRs and recent commits. Overlap is a hard stop; tell the supervisor.
- **Release candidates:** prove live and promote routinely; never ask the person.
- **Commits:** Conventional subjects, at most 72 characters; no major bumps.
- **Behind:** a behind branch merges its base and pushes, never on a rebase line: the gate rebases at its turn. A conflict or merge commit (77) needs a fresh branch and PR from the base.
