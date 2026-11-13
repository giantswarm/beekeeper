---
name: review
description: Independently review a pull request for fulfilment before the supervisor permits merge. Use in a fresh reviewer session, never to review your own change.
---

Start with `beekeeper agents review <owner/repo>#<n>` in the repository checkout (or configure `agents.dir`/`agents.roots`). CI must be green at the current head. `--model` overrides `supervisor.reviewModel` (default `opus`, the strongest Claude alias). The verdict prefix stays fixed across models. The command pins PR/head/issues/parent epic/round. A pull request gets one desk review: the command refuses once it has a desk verdict, on any head.

Read the filled brief, PR, issue comments and parent epic. Check:

- Every acceptance line of the issue and parent is met in full; no minimum-CI interpretation.
- No narrowing or deferral without a linked follow-up the issue allows.
- Tests cover the requested behaviour, including failures, rather than just the happy path.
- In giantswarm/beekeeper, a fix or behaviour change of the core (Go code under `cmd/`, `internal/`, `pkg/`) adds or extends a scenario under `internal/scenario/scenarios/` that fails without it, a bug's naming its issue; CI's `scenario-control` step proves the failure on the merge base. Reject a core fix without one, unless the PR says why it cannot be a scenario and links the harness follow-up. The same step proves a Go regression test: of the `Test…` functions the PR adds or changes, at least one must fail on the merge base with the PR's test files applied and pass on the PR; its report (`go-control/report.txt`) lists each test's run and the packages that do not build on the base. A test the PR calls its regression test must be one that fails there. Each `control: {none: <reason>}` of a scenario, or `control: none: <reason>` of a test's doc comment, the PR writes must name why it cannot fail on the merge base; never accept one on a fix's own scenario or regression test.
- No dead paths, unsupported claims in the PR or docs, or work a later epic sub-issue must undo.
- Behaviour the PR adds or changes is documented in its `docs/` topic page, not appended to README.md.

Use the light brief for fewer than `supervisor.reviewLightLines` changed lines (default 150; `--light-lines` overrides; negative disables this shortcut), or docs/tests-only changes: diff, issue and CI, with no checkout or build. Otherwise create your own worktree at the pinned head and read repository rules for tests and lints. Trust green CI on that exact head. Do not run local tests, lints, builds or probes; inspect code and tests instead. Never merge.

This is the pull request's one desk review; no second review runs for it. Every finding is blocking or a nit to fix in this round; drop anything else. Fix your own findings yourself in your worktree at the pinned head (a light review creates one only for its fixes) in code, tests or docs, commit them as one commit, push it once to the pull request's branch (`git push origin HEAD:refs/heads/<branch>`, never force-push), and approve the head you pushed at once, without waiting for its CI: the loop waits for green CI on that head and merges it. Never merge the base into the branch or rebase it to bring it up to date: the loop merges the approved head as it is, and its merge-result check against the current base is the gate. Request changes only for a finding that needs a product or design decision or cannot be fixed on the branch, or when your publication is refused: the worker fixes those, and the loop merges its green fix without another review. A new issue is only for a real gap outside the PR or a deferral the issue permits: label it `follow-up`, leave it unranked in backlog, never attach it as an epic sub-issue. `board next` skips `follow-up` by default.

Post exactly one comment review using `gh pr review --comment --body-file <file>`, never `--approve`. Its first line is exactly one of:

```
Desk review (opus): approved
Desk review (opus): changes requested
```

Its second line is `Head reviewed: <sha>` (put the SHA in backticks: the pinned head, or the full SHA of the head you pushed when you fixed findings, even while its CI runs). Each finding is `<file>:<line>: <what is wrong> → <what would be right>`, saying which you fixed. Report that same verdict and the review URL to the supervisor with `beekeeper agents idle --done --report "<verdict, newline, review URL>" --problem none`, or report each encountered problem with evidence and owning repository.

Never merge. After your approval the loop gives the merge go for the head you approved once its CI is green; it relays findings you could not fix to the worker and merges the worker's green fix without a second review. A later head that only brings the base into the approved one (a merge, a conflict's resolution, a rebase) keeps the approval. Nobody updates an approved branch with the base: only a real merge conflict is resolved on it. A conflict or red CI after approval returns to the reviewer for a repair turn, without another review verdict; push the repair and park for the loop to wait for CI and merge.

For out-of-scope findings, check the owning repository's open issues first. A known finding adds
nothing. New facts rewrite the existing issue's description, never comments. Only a new finding
gets a new issue; include its full URL in the problem report.
