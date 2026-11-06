---
name: github
description: Load before GitHub scans, builds, browser or waits.
---

# github — GitHub

- **Budget:** wait for reset if `beekeeper budget --gate` refuses; code search adds
  `--bucket code_search`.
- **Personal-account GraphQL:** `GH_REPO=<owner/repo> gh api graphql`.
- **Membership:** `beekeeper person <login>`, not `gh api` members endpoints: they misread private members.
- **Issue/PR scans:** list each repo (`gh issue list --repo <owner/repo>`, `gh pr list` likewise),
  never multi-repo `gh search`: it drops private repos. Unreadable is never "no match".
- **Code search:** 10 requests per minute. Across more than a few
  repos, use `git clone --depth 1` under `<stateDir>/code-scans/<owner>/<repo>` and `rg`,
  never per-repo `gh search code` loops. `stateDir`: configured state directory.
- **Code scans** (govulncheck, trivy): fresh worktree of `origin/main`, never a shared clone;
  a finding names the commit and module versions.
- **Headless** (first or `wake`): foreground waits (`devctl pr wait`, bounded Bash);
  background waits never wake them. End done or parked (`beekeeper:park`).
- **No @-mention**: pings via `note add` to the supervisor.
- **Builds/tests/lints**: `beekeeper run` (hook does it). Exit 75: no slot, rerun in
  background, never a loop. Exit 137: cap, bound parallelism. Avoid heavy work under
  `beekeeper watch` load or memory pressure.
- **Browser / Dependabot:** before either, read [references/browser-and-security.md](references/browser-and-security.md).
- **Subagents:** brief must allow; tell each force-push and secret rules.
