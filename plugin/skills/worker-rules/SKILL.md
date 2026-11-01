---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules — a worker on a beekeeper desk

A worker works one task to the finish. The desk's stricter AGENTS.md, CLAUDE.md and rules win.

- **Goal.** Merged, released, rolled and proven live, in one report (PR links, versions, proof, open items, live facts, "Problems found") by `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (each, or `none`). Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`.
- **The board holds the order**: priority, holds, plans, answers; next: `board next`.
- **The supervisor** (`beekeeper supervisor status`): report via `agents idle --done --report`, park via `agents park`, ask via `note add`, wake via the inbox; `agents register` joins the roster.
- **Desk review.** After pushing and green CI, run `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"` in one shell call. The published worktree's branch and HEAD prove the review request; a park from another directory or an unpushed head is a plain wait with no reviewer, go or findings forwarded. Re-run from the published worktree if the command says plain wait.
- **Problems found reach the supervisor**: what broke, evidence, owning repo, issue URL if filed. The supervisor files the rest (a worker if told). Never bypass a broken gate or CI: report first, naming a detour.
- **Never ask the person.** No question tool, prompt, browser approval or setting wait: ask the supervisor for the guide via `beekeeper note add` and `agents park --on '#<note>'`; the relayed answer is the person's word. Report a barrier, carry on. Ask about limits, cost and accounts, never continuing or setup: pick the default, say so.
- **Our own Apps** (`beekeeper app`): settings, installs and consents use `beekeeper browse`, never the person; consenting to another's App needs their word.
- **Secrets.** Never read, print or compare a value, even hashed, but by `beekeeper secret compare|fingerprint`: keys and metadata only. Every cluster call names its context/kubeconfig; never write to `kube.production`.
- **Alerts.** Post-roll and proof checks use `KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts snapshot`, never Alertmanager/Mimir.
- **Git.** No force push, not `--force-with-lease`: a behind branch merges its base and pushes, never on a rebase line. Work in your worktree; no `git stash`: commit waiting work.
- **Files.** Scratch goes in `<scratchpad>` or `~/.local/state/<program>/`, never `~`, `~/projects` or a main checkout; `cd "${dir:?}"`, `>|`.
- **No polling**: no `--watch` or sleep loop; `devctl pr wait|merge` block, never under `timeout`; an agentlab proof only waits: never under `beekeeper run`.
- **Nothing left running** (port-forward, monitor, background wait); kill by PID, a gated run by `beekeeper gate stop`. Never hard-delete: move aside.
- **Near `agents.relayAt`**, record a handover (`agents note`), stop.

Load the step's skill first: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`,
`beekeeper:park`.
