---
name: review
description: Independently review a pull request for fulfilment before the supervisor permits merge. Use in a fresh reviewer session, never to review your own change.
---

Start with `beekeeper agents review <owner/repo>#<n>` in the repository checkout (or configure `agents.dir`/`agents.roots`). CI must be green at the current head. `--model` overrides `supervisor.reviewModel` (default `opus`, the strongest Claude alias). The verdict prefix stays fixed across models. The command pins PR/head/issues/parent epic/round/previous findings and refuses a second review of that head.

Read the filled brief, PR, issue comments and parent epic. Check:

- Every acceptance line of the issue and parent is met in full; no minimum-CI interpretation.
- No narrowing or deferral without a linked follow-up the issue allows.
- Tests cover the requested behaviour, including failures, rather than just the happy path.
- In giantswarm/beekeeper, a fix or behaviour change of the core (Go code under `cmd/`, `internal/`, `pkg/`) adds or extends a scenario under `internal/scenario/scenarios/` that fails without it, a bug's naming its issue; CI's `scenario-control` step proves the failure on the merge base. Reject a core fix without one, unless the PR says why it cannot be a scenario and links the harness follow-up. The same step proves a Go regression test: of the `Test…` functions the PR adds or changes, at least one must fail on the merge base with the PR's test files applied and pass on the PR; its report (`go-control/report.txt`) lists each test's run and the packages that do not build on the base. A test the PR calls its regression test must be one that fails there. Each `control: {none: <reason>}` of a scenario, or `control: none: <reason>` of a test's doc comment, the PR writes must name why it cannot fail on the merge base; never accept one on a fix's own scenario or regression test.
- No dead paths, unsupported claims in the PR or docs, or work a later epic sub-issue must undo.
- Behaviour the PR adds or changes is documented in its `docs/` topic page, not appended to README.md.

Use the light brief for fewer than `supervisor.reviewLightLines` changed lines (default 150; `--light-lines` overrides; negative disables this shortcut), or docs/tests-only changes: diff, issue and CI, with no checkout or build. Otherwise create your own worktree at the pinned head and read repository rules for tests and lints. Trust green CI on that exact head. Do not run local tests, lints, builds or probes; inspect code and tests instead. Never merge.

Every finding is blocking or a nit to fix in this round; drop anything else. Review only: never edit, commit or push fixes to the reviewed branch. Post findings and request changes; report the verdict and review URL with `beekeeper agents idle --done --report` so the supervisor forwards them to the implementing worker. Approve only the assigned, unchanged head when it needs no fixes. A new issue is only for a real gap outside the PR or a deferral the issue permits: label it `follow-up`, leave it unranked in backlog, never attach it as an epic sub-issue. `board next` skips `follow-up` by default.

Post exactly one comment review using `gh pr review --comment --body-file <file>`, never `--approve`. Its first line is exactly one of:

```
Desk review (opus): approved
Desk review (opus): changes requested
```

Its second line is `Head reviewed: <sha>` (put the SHA in backticks: the assigned head only). Each finding is `<file>:<line>: <what is wrong> → <what would be right>`. Report that same verdict and the review URL to the supervisor with `beekeeper agents idle --done --report "<verdict, newline, review URL>" --problem none`, or report each encountered problem with evidence and owning repository.

Never merge. The supervisor relays findings to the worker; it gives the merge go only after approval. An unchanged approved head gets no second review. For a push fixing only approval nits, the supervisor checks the delta against the approved head, then runs `beekeeper --as <supervisor> agents review <owner/repo>#<n> --nits-of <approved-full-sha>` to attest that check before the loop gives an exact-head go; it must not silently carry an approval across arbitrary code changes.
