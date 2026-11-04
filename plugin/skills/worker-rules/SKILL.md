---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — reports, supervisor, problems, secrets, git. Load for every supervisor task; `agents start` sends them as the first message.
---

# worker-rules — a worker on a beekeeper desk

One task; stricter rules win.

- **Goal.** Merge/release/roll/prove. Report PRs/versions/proof/open items/problems: `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (each, or `none`). Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`.
- **Board** holds/plans/answers; blocked: `board move <n> blocked --by <issue>`.
- **Supervisor** (`supervisor status`): park `agents park`, ask `note add`, no status to the guide; inbox wakes, register `agents register`.
- **Promotion.** One promoter per line, named by the supervisor; only they promote. A stable whose unpromoted RCs carry a teammate's change needs that teammate's one-line yes on their PR.
- **Review.** Push/green CI: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> --post-merge "<remaining work>" "desk review"`. Same HEAD; loop merges; `--post-merge` unless merge ends task.
- **Problems**: `agents problem "<finding>"` sends evidence/repo/issue URL; supervisor files. Gate/CI: report detour first; never bypass.
- **Never ask the person.** No prompts/browser approval/setting waits. Guide: `beekeeper note add`, then `agents park --on '#<note>'`; relays are the person's word. Report barriers. Limits/cost/accounts: ask; setup/continuing: state default.
- **Apps** (`beekeeper app`): settings/installs/consents: `beekeeper browse`, never the person; another's App needs their word.
- **Secrets.** Values, even hashed: only `beekeeper secret compare|fingerprint`: keys/metadata only. Cluster: name context/kubeconfig; never write `kube.production`.
- **Alerts.** Post-roll/proof: `KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts snapshot`, never Alertmanager/Mimir.
- **Rounds.** Focused checks; one push/round. Read failures; rerun failures (`-run`); `set -o pipefail`. Review proofs; CI: full checks.
- **Git.** No force push (even `--force-with-lease`). Before approval a behind branch merges its base and pushes, never on a rebase line; then the merge does. Own worktree; commit, no `git stash`.
- **Files.** Scratch goes in `<scratchpad>` or `~/.local/state/<program>/`, never `~`, `~/projects`, main checkouts; `cd "${dir:?}"`, `>|`.
- **No polling**: no `--watch`/sleep loops; `devctl pr wait|merge` block, never under `timeout`; an agentlab proof only waits: never under `beekeeper run`.
- **Stop**: kill by PID, gated runs by `beekeeper gate stop`. Never hard-delete: move aside.
- **Near `agents.relayAt`**, handover (`agents note`), stop.

Load: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`, `beekeeper:park`.
