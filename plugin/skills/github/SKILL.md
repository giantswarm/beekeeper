---
name: github
description: GitHub reads, builds, browser steps and waits on a beekeeper desk: the budget gate, scans, beekeeper run, headless turns, browse. Load before them.
---

# github — GitHub, builds, the browser and waits

- **Budget:** stop while `beekeeper budget --gate` refuses, until reset; code search adds
  `--bucket code_search`.
- **Membership:** `beekeeper person <login>`, never `gh api` members endpoints: private members
  are misread.
- **Dependabot alerts:** before/after remediation, `beekeeper security alert <owner/repo> <number>`.
  Unreadable or dismissed is not fixed. App: Dependabot alerts read, accepted on installation.
  No personal scopes or security-scan bypass.
- **Scans:** list each repo (`gh issue list --repo <owner/repo> --search "<query>"`, `gh pr list`
  likewise), never multi-repo `gh search`, which drops private repos. Unreadable is a problem,
  never "no match".
- **Code scans** (govulncheck, trivy): fresh worktree of `origin/main`, never a shared clone;
  a finding names the commit and module versions.
- **Headless turns** (a first or `wake` turn): foreground waits (`devctl pr wait`, bounded Bash);
  background waits never wake them. End done or parked (`beekeeper:park`).
- **No @-mention** (the hook refuses one): pings via `note add` to the supervisor.
- **Builds**, tests, lints: `beekeeper run` (the hook does it). Exit 75: no slot, rerun in
  background, never a loop. Exit 137: cap, bound parallelism. No heavy work under
  `beekeeper watch` load or memory pressure.
- **Browser:** headless uses CLI Claude in Chrome tools; desktop uses `beekeeper browse "<steps>"`.
  Never wait on site approval. Record own App grants first (`beekeeper app allow`); refusals
  are barriers, never the person's.
- **Subagents:** only if the brief allows, each told force-push and secret rules.
