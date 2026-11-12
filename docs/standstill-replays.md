# Standstill replays

Before closing a standstill, retain its planner input and the transitions that
must remain possible. Capture a read-only pass with:

```sh
beekeeper supervisor loop --once --dry-run --record incident
```

This creates `incident/input.json`, refusing to overwrite existing evidence.
Identities become consistent placeholders; arbitrary text, paths and unknown
string fields are removed. Nonempty tasks and continuation events become fixed
markers. The recording contains the pure planner input, agents, deliveries and
available slots, not the desk configuration. Live query callbacks are replaced with the facts they returned during planning. Textual
event grammars are not retained: reduce them to synthetic events when needed.
Structured action keys retain their anonymized heads, sweep markers and tracking
episodes so recorded review rounds and completed merge grants remain replayable.
Delivery reservations use the same derived observations as the dry-run preview.
Review the recording before publishing it and retain only the data the incident
needs. This is a structural replay, not a backup of the desk.

Put it under `internal/superloop/testdata/standstills/<issue>/input.json`, beside
`want.json`. Expectations are lower bounds (`Starts`, `Actions` by planner kind)
and named `Continuations`. `TestStandstillReplay` runs every directory through the
real pure planner, reservation and continuation paths. The first two fixtures
are reduced reproductions of the documented reservation pileup and missing
parked continuations; they are not claimed to be historical live snapshots.

The CI loop-workflow job runs the fixtures on the candidate, then puts back the
uncapped reservation subtraction and disabled parked continuation admission, one
at a time. It requires a specific behavioral assertion failure, not a build
failure. Logs are retained under the job's `loop-workflow/standstills` artifacts.
Add a corresponding negative control for each new standstill before closing it.

Self-update compares the running and staged binaries' read-only live passes
before replacement, one after the other, each with a 2-minute deadline. Unlike
the recurring loop, a one-shot pass reads beyond the normal sweep batch limit;
its deadline and GitHub admission gates still apply. The candidate is refused
when its pass fails, when it cannot observe the desk (it runs out of time, its
GitHub reads are held or its observations are incomplete) while the running
release's pass completed, or when it loses all starts, continuations, reviews
or merges present in the running release. A refusal of a pass that fails or
cannot observe names that pass's own last line; a lost kind names the kind and
the running release's count. The running release's pass is only the baseline:
when it cannot complete, for whatever reason, nothing is compared and only a
failing candidate pass refuses, so a release whose dry-run breaks, or a desk
whose GitHub reads keep failing, never holds back a candidate (and with it the
fix).
Self-update then says, and logs as `self-update.comparison`, that the
candidate went uncompared and why; signature verification, the other canary
checks and probation still apply. Comparison refusals do not enter the skipped-release or
quarantine files and can be retried after an outage.
`self-update --allow-loop-regression` explicitly skips this comparison for an
intentional planner change; signature verification and the other canary checks
still apply. The two passes observe live state minutes apart, so legitimate
state movement can cause a conservative refusal; retry before overriding it.
