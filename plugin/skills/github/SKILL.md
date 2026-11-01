---
name: github
description: GitHub reads, scans, builds, browser steps and waits on a beekeeper desk. Load before them.
---

# github — GitHub, builds, the browser and waits

- **Budget.** Stop while `beekeeper budget --gate` refuses, until reset; code search adds
  `--bucket code_search`.
- **Membership:** `beekeeper person <login>`, never `gh api` members endpoints, which misread
  private members.
- **Security scans.** Read Dependabot state before and after remediation:
  `beekeeper security alert <owner/repo> <number>`. Unreadable is not fixed; dismissal is not a
  fix. The App needs Dependabot alerts: read, accepted on its installation. Never add personal
  token scopes or bypass a failed security scan.
- **Scans:** list each repo (`gh issue list --repo <owner/repo> --search "<query>"`, `gh pr list`
  likewise), never multi-repo `gh search`, which drops private repos. An unreadable listing is
  a problem, never "no match".
- **Code scans** (govulncheck, trivy): fresh worktree of `origin/main`, never a shared clone;
  a finding names the commit and module versions.
- **Headless turns:** wait in the foreground (`devctl pr wait`, bounded Bash); a background
  wait never wakes the turn. End done or parked (`beekeeper:park`).
- **No @-mention** in posts: pings go via `note add` to the supervisor.
- **Builds, tests, lints:** `beekeeper run`. Exit 75: no slot, rerun in background, never a
  loop. Exit 137: cap reached, bound parallelism. No heavy work under `beekeeper watch` load
  or memory pressure.
- **Browser:** headless turns use CLI Claude in Chrome tools; desktop turns use
  `beekeeper browse "<steps>"`. Never wait on site approval. Record own App grants first
  (`beekeeper app allow`); refusals are barriers, never the person's.
- **Subagents:** only if the brief allows, each told force-push and secret rules.
