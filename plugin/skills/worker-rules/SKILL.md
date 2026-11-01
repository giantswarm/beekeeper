---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules — a worker on a beekeeper desk

One task; stricter desk rules win.

- **Goal.** Merged/released/rolled/proven live. Report PRs/versions/proof/open items/live facts/"Problems found": `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (each, or `none`). Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`.
- **Board** priority/holds/plans/answers: `board next`.
- **Supervisor** (`beekeeper supervisor status`): report `agents idle --done --report`, park `agents park`, ask `note add`; inbox wakes, register `agents register`.
- **Desk review.** After push and green CI, in one call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. Published branch/HEAD must match; re-park there if plain.
- **Problems** to the supervisor: evidence/owning repo/issue URL; it files the rest. Broken gate/CI: report first, name detour; never bypass.
- **Never ask the person.** No prompt/question tool/browser approval/setting wait. Ask the guide: `beekeeper note add`, then `agents park --on '#<note>'`; relayed answers are the person's word; merging: `answer verify` exit 0. Report barriers; continue. Limits/cost/accounts: ask; setup/continuing: state default.
- **Apps** (`beekeeper app`): own settings/installs/consents: `beekeeper browse`, never the person; another's App needs their word.
- **Secrets.** Never read, print or compare a value, even hashed, but by `beekeeper secret compare|fingerprint`: keys/metadata only. Cluster calls name context/kubeconfig; never write `kube.production`.
- **Alerts.** Post-roll/proof: `KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts snapshot`, never Alertmanager/Mimir.
- **Rounds.** Focused checks, push fixes together once per round. Read saved failures; rerun failing tests only (`-run`); pipes: `set -o pipefail`. Reviews: focused reproductions, full checks are CI's.
- **Git.** No force push (`--force-with-lease` too): a behind branch merges its base and pushes, never on a rebase line. Own worktree; commit, no `git stash`.
- **Files.** Scratch goes in `<scratchpad>` or `~/.local/state/<program>/`, never `~`, `~/projects`, main checkouts; `cd "${dir:?}"`, `>|`.
- **No polling**: no `--watch`/sleep loops; `devctl pr wait|merge` block, never under `timeout`; an agentlab proof only waits: never under `beekeeper run`.
- **Stop**: kill by PID, gated runs by `beekeeper gate stop`. Never hard-delete: move aside.
- **Near `agents.relayAt`**, handover (`agents note`), stop.

Load first: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`, `beekeeper:park`.
