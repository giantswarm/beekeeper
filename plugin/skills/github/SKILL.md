---
name: github
description: GitHub reads, builds, browser steps and waits on a beekeeper desk: the budget gate, scans, beekeeper run, headless turns, browse. Load before them.
---

# github — GitHub, builds, the browser and waits

- **Budget.** GitHub work stops while `beekeeper budget --gate` refuses, until the reset; code
  search adds `--bucket code_search`.
- **Membership** is `beekeeper person <login>`, never a members endpoint of `gh api`, which
  misreads a private member.
- **Scans list each repository** (`gh issue list --repo <owner/repo> --search "<query>"`, `gh pr
  list` likewise), never one `gh search` over several, which drops a private repository. One
  the listing cannot read is a problem found, never "no match".
- **A code scan** (govulncheck, trivy) runs in a fresh worktree of `origin/main`, never a shared
  clone; a finding names the commit and module versions.
- **A headless turn** (a first or `wake` turn) never ends on a background wait, which never wakes
  it: it waits in the foreground (`devctl pr wait`, a bounded Bash) and ends done or
  parked (`beekeeper:park`).
- **No @-mention** in a post (the hook refuses one): a ping goes via `note add` to the supervisor.
- **Builds**, tests and lints run through `beekeeper run` (the hook does it): exit 75 is no slot,
  rerun in the background, never in a loop; exit 137 is the cap, bound the parallelism. No heavy
  work while `beekeeper watch` reports load or memory pressure.
- **The browser never waits on a site approval.** A headless turn has the CLI's Claude in Chrome
  tools; a desktop turn, whose browser tools are refused, runs its steps through `beekeeper browse
  "<steps>"`. Our own App's grant is recorded first (`beekeeper app allow`); a refusal is a
  barrier, never the person's.
- **Subagents** only when the brief allows them, each told the force-push and secret rules.
