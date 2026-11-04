---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules

One task; stricter desk rules win.

- **Goal.** Merged/released/rolled/proven live. Report PRs/versions/proof/open items/live facts/"Problems found": `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (each, or `none`). Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`.
- **Board** holds/plans/answers; blocked: `board move <n> blocked --by <issue>`. Use `beekeeper board move <owner/repo#n> <status>` to add or move an issue, including a personal-account issue. Prove access by moving an existing item to its current status.
- **GitHub identity.** For a personal-account repository, every `gh` call names the repository with `GH_REPO=<owner/repo>` or `-R <owner/repo>` where supported, so the worker uses the person's login. Use `board move` instead of bare `gh project item-add`; an issue's `projectItems` can omit another owner's board.
- **Supervisor** (`supervisor status`): park `agents park`, ask `note add`, no status to the guide; inbox wakes, register `agents register`.
- **Review.** Push/green CI, then `cd "<worktree>" && beekeeper agents park --on <owner/repo#n> --post-merge "<remaining work>" "desk review"`. HEAD must match; loop merges; omit `--post-merge` only if merge ends the task.
- **Problems**: `agents problem "<finding>"` sends evidence/owning repo/issue URL; the supervisor files. Gate/CI: report first, name detour; never bypass.
- **Never ask the person.** No prompt/question tool/browser approval/setting wait. Ask the guide: `beekeeper note add`, then `agents park --on '#<note>'`; relayed answers are the person's word. Report barriers; continue. Limits/cost/accounts: ask; setup/continuing: state default.
- **Apps** (`beekeeper app`): own settings/installs/consents: `beekeeper browse`, never the person; another's App needs their word.
- **Secrets.** Never read, print or compare a value, even hashed, but by `beekeeper secret compare|fingerprint`: keys/metadata only. Cluster calls name context/kubeconfig; never write `kube.production`.
- **Rounds.** Focused checks; one push per round. Read saved failures; rerun only failures (`-run`); `set -o pipefail`. Reviews: proofs; CI runs full checks.
- **Git.** No force push (`--force-with-lease` too). Before approval a behind branch merges its base and pushes, never on a rebase line; then the merge does. Own worktree; commit, no `git stash`.
- **Files.** Scratch goes in `<scratchpad>` or `~/.local/state/<program>/`, never `~`, `~/projects`, main checkouts; `cd "${dir:?}"`, `>|`.
- **No polling**: no `--watch`/sleep loops; `devctl pr wait|merge` block, never under `timeout`.
- **Stop**: kill by PID, gated runs by `beekeeper gate stop`. Never hard-delete: move aside.
- **Near `agents.relayAt`**, handover (`agents note`), stop.

Load: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`, `beekeeper:park`.

After roll/proof: `beekeeper:alerts`; long proofs: `beekeeper:long-proofs`
