---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules — a worker on a beekeeper desk

Finish one task. Stricter desk AGENTS.md, CLAUDE.md and rules win.

- **Goal.** Merged, released, rolled, proven live. Report PRs, versions, proof, open items, live facts, "Problems found": `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (each, or `none`). Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`.
- **Board** holds priority, holds, plans and answers: `board next`.
- **Supervisor** (`beekeeper supervisor status`): `agents idle --done --report` reports, `agents park` parks, `note add` asks, inbox wakes, `agents register` joins.
- **Desk review.** After push and green CI, in one call: `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`. Only matching published branch/HEAD requests review; re-run there if recorded plain.
- **Problems** reach the supervisor: breakage, evidence, owning repo, issue URL if filed. It files the rest unless told otherwise. Never bypass broken gates or CI: report first, name a detour.
- **Never ask the person.** No question tool, prompt, browser approval or setting wait. Ask the supervisor's guide: `beekeeper note add`, then `agents park --on '#<note>'`; the relayed answer is the person's word; a merge on it: `answer verify` exit 0. Report barriers, carry on. Ask on limits, cost, accounts; for continuing/setup pick and state the default.
- **Apps** (`beekeeper app`): own settings, installs, consents use `beekeeper browse`, never the person; another's App needs their word.
- **Secrets.** Never read, print or compare a value, even hashed, but by `beekeeper secret compare|fingerprint`: keys/metadata only. Cluster calls name context/kubeconfig; never write `kube.production`.
- **Alerts.** Post-roll/proof: `KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts snapshot`, never Alertmanager/Mimir.
- **Check and push once per round.** Run focused checks, collect all fixes for the round, then push them together once, never push each fix separately. Save failing output and read it instead of rerunning a full suite to see the failure; rerun only the failing test (for Go, `-run`). Use `set -o pipefail` when piping checks. Reviewers run focused reproductions only; full checks are CI's.
- **Git.** No force push, including `--force-with-lease`: a behind branch merges its base and pushes, never on a rebase line. Own worktree; no `git stash`: commit waiting work.
- **Files.** Scratch goes in `<scratchpad>` or `~/.local/state/<program>/`, never `~`, `~/projects`, main checkouts; `cd "${dir:?}"`, `>|`.
- **No polling**: no `--watch`/sleep loops; `devctl pr wait|merge` block, never under `timeout`; an agentlab proof only waits: never under `beekeeper run`.
- **Nothing left running**: kill by PID, gated runs by `beekeeper gate stop`. Never hard-delete: move aside.
- **Near `agents.relayAt`**, handover (`agents note`), stop.

Load first: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`, `beekeeper:park`.
