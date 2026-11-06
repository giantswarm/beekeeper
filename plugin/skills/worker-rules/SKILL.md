---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules

Stricter wins.

- **Goal.** Merge/release/roll/prove; report PRs/versions/proof/open items/live facts/problems: `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (each or `none`). Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`.
- **Board** holds/plans/answers; blocked: `board move <n> blocked --by <issue>`.
- **Desk** (`supervisor status`): park `agents park`, ask `note add`; no guide status; inbox wakes; register.
- **Review.** Push/green CI: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> --post-merge "<remaining work>" "desk review"`. Same HEAD; loop merges; `--post-merge` unless merge ends task.
- **Promote.** One supervisor-named promoter per line. Stable carrying a teammate's unpromoted RC needs their yes on its PR.
- **Problems**: `agents problem "<finding>"`: evidence/repo/issue URL. Gate/CI: report first. Desk rules/notes for system faults are an antipattern: fix the core; desks hold personal choices.
- **Never ask the person.** No prompts/browser approval/setting wait. Guide: `beekeeper note add`, `agents park --on '#<note>'`; relays are the person's word. Limits/cost/accounts: ask; setup: default.
- **Apps**: settings/installs/consents: `beekeeper browse`, never the person; another's App needs their word.
- **Post-merge.** `Part of <full issue URL>`, no auto-close; close with proof.
- **Secrets.** Never read, print or compare a value, even hashed, but by `beekeeper secret compare|fingerprint`: keys/metadata only. Cluster: name context/kubeconfig; never write `kube.production`.
- **Tests.** Focused checks; 1 push/round; read failures, rerun (`-run`); `set -o pipefail`. Review proofs; CI checks all.
- **Git.** No force push (even `--force-with-lease`). Before approval a behind branch merges its base and pushes, never on a rebase line; then the merge does. Own worktree; commit, no stash.
- **Files.** Scratch goes in `<scratchpad>` or `~/.local/state/<program>/`, never `~`, `~/projects`, main checkouts; `cd "${dir:?}"`, `>|`.
- **No polling**: no `--watch`/sleep loops; `devctl pr wait|merge` block, never under `timeout`.
- **Stop**: kill by PID; gated: `beekeeper gate stop`. Move aside; no hard-delete.
- **Near `agents.relayAt`**, handover (`agents note`), stop.

Load: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`, `beekeeper:park`.

Post-roll/proof: `beekeeper:alerts`; long proofs: `beekeeper:long-proofs`
