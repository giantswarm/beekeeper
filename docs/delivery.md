# Delivery cycle and work in progress

A delivery is one issue from its worker's start until its merge has a
confirmed release. A pull request closing several issues counts once for each
issue. An owned pull request with no linked issue counts once under its own
reference. Other people's pull requests do not count.

`beekeeper delivery` shows the current step, step age, total age, worker,
linked pull requests and waits. `supervisor status` and `report` relay this
same reading. The steps follow the supervisor's existing cycle:

| Step | Observation |
| --- | --- |
| implementing | A worker started the issue. |
| ci | The published head is parked for review; CI is pending. |
| review-queued | CI is green and a reviewer is waiting for room. |
| reviewing | An independent reviewer is running. |
| fixing | Findings, failed checks or a conflict require worker changes. |
| merging | The reviewed head has its go and is in the merge lane. |
| releasing | The merge landed; its release is not yet confirmed. |
| done | A containing release tag is available, or no release is required. |
| dropped | Work closed unmerged, finished without a merge, or was dropped by hand. |
| unknown | An observation is incomplete; its place remains occupied. |

An available release candidate counts as released. Promotion, installation
and post-merge live proof follow outside this limit. Failed or unconfirmed
releases remain tracked and are escalated. `delivery.step` events retain each transition,
including its original start time, for cycle evidence and statistics.

## Limit and capacity

Set `delivery.wipLimit` with the component's configuration command:

```sh
beekeeper config set delivery.wipLimit 2
```

Zero disables the limit while keeping observation and statistics active.
Capacity still controls running sessions through its existing resource and
subscription constraints. Both must permit an ordinary new issue to start.
A parked worker frees session capacity. Only **flowing** deliveries keep a
WIP place: implementation, CI, review, merge, release and automatic timers
under a day. An open decision addressed to the guide's person and linked to
the delivery (or its worker's note park) is **waiting on the person** and
appears in the existing guide queue. A missing decision note is reported as
a defect; file it with `note add --for <person> --ref <issue>` and include
the question, default and deadline.

Use `agents park --external "<what must happen>"` for any external event
that no current decision can resolve. It needs an explicit wake when the
event occurs. This typed wait is supported by local and execution workers.
Existing parks naming a physical visit, technician, external party or
physical measurement are recognized as external waits too. A timer
of at least a day, or a dependency on a delivery that is not flowing is
**blocked**. The loop records a board hold with its reason, moves the issue
to Blocked / Waiting and restores its previous status when the wait clears.
A person's visit without an actionable decision belongs here; a decision
about arranging a visit belongs in a note. Unknown waits remain flowing
until described clearly, so ambiguity cannot silently open admission.
Running reviewers and fixers keep a delivery flowing even while its original
worker waits. Blocked and person waits reserve no new worker slot.

`supervisor status` prints `N flowing of limit L; M waiting on the person;
K blocked`. `delivery --json` includes each row's `flow` classification and
reason. Only the flowing count enters admission. A merge alone also keeps
its place until release. An already over-limit desk drains existing work
before admitting another ordinary issue.

The gate covers automatic fill, next-after-done, monitored issues, board
selection and claims, and the final dispatch. Claims count under the state
lock, so simultaneous starts cannot each spend the last place. A full limit
leaves slots empty instead of reading more ordinary board work. Incomplete sweeps reuse retained delivery observations, including local starts.
Only a complete observation older than the greater of ten minutes and three
loop intervals (or no complete observation) holds new work for freshness.

Reviews, fixes, wakes and merges continue at the limit. Pending reviewers
and fixes without a live worker reserve capacity before ordinary new work.
Manual starts, the named repair of an active incident, and `board.order`
steps with `wipExempt: true` can exceed the limit, but still count. This is
why the count can exceed the configured value without an admission failure.

Only owned active deliveries with a live worker, reviewer or fixer, or a recent
step change, count. Without a live agent, review and fix steps leave the count
after `supervisor.reserveFor` (default one hour); other steps use
`delivery.stallAfter` (default 24 hours). Stalled records remain visible and are
raised once per episode to the guide, with recovery reported once.
Use `beekeeper delivery drop <ref> --reason <why>` only to end its tracking:
it neither closes pull requests nor stops workers. A changed pull head
reopens a dropped delivery.

## Choosing the first value

Observe with the limit off, then read a consistent statistics window:

```sh
beekeeper delivery --stats --since 168h
beekeeper delivery --stats --since 168h --json
```

Multiply released per day by mean start-to-release time **in days**, then
round down. The mean uses complete, nonnegative cycles released inside the
window, preserving their full age even if they started before the window.
Missing start timestamps do not contribute a cycle sample. Step residence
statistics are clipped to the window and include unfinished work; adding
step medians does not give the mean cycle time. If there are no complete
cycles, collect more evidence before selecting a value. A computed zero
needs further observation because zero disables admission control.

Before and after setting the limit, compare `delivery` with the open pull
requests. They need not match: deliveries include pre-PR implementation and
unreleased merges, combine linked issues, and exclude unowned PRs. Record
these differences instead of treating PR count as the delivery count.

## End-to-end acceptance evidence

`TestSupervisorWorkflowDeliveryLimit` in `internal/loopworkflow` drives the
public candidate CLI against a scratch Git origin, four ready board items
and external tool adapters. CI verifies a limit of two despite four free
slots, idle slots while full, review and a fix ahead of issue three, and a
merge that cannot free a place until release is observed. The next pass
admits issue three after that release.

After a release containing the whole feature, observe at least one real
cycle. Record the release version, `delivery` readings, one issue's
`delivery.step` events from implementing to done, the review and fix while
full, and the next ordinary start after release. Compare open PR counts
across the full interval, accounting for exempt starts. Capture alerts via
`beekeeper alerts snapshot` and report its findings alongside the proof.
Keep public evidence generic: no local configuration, machine identifiers,
private references or credentials. The epic stays open until this live
acceptance evidence is complete.

### Worker task outcomes

A delivery records the worker's outcome and its evidence independently of
its release step. A worker finishes with one of these commands:

- `agents idle --done --report "..." --problem none`: each owned delivery
  must have a linked pull request that GitHub confirms is merged. The fresh
  session the loop starts for a parked worker's task (its post-merge work, for
  one) continues that task's delivery: a delivery still active or ended merged
  keeps its pull requests and outcome, and the pull request the worker parked
  on joins them, also when it only says "Part of" the issue. Its own
  `--done` then rests on that merge without a park of its own. A worker whose
  brief names its task by a pull request URL (no issue) works that pull
  request: its delivery holds the pull request from the start, as the loop's
  observation of an issue-less pull request has it, so its merge proves the
  task although the loop's sweep of the desk's open pull requests never read
  it (another author's, or merged before a sweep).
- `agents idle --done --no-change "<reason and proof>" --report "..." --problem none`:
  explicitly explain an already fixed issue, an operation with its proof, or
  an answer recorded on the issue. This counts as no change, not a release.
- `agents idle --blocked --by <issue URL|#note>`: the blocker must be open.
  An issue dependency moves the item to Blocked / Waiting; a note holds its
  board status until the note is answered. This ends the worker assignment,
  not the blocked issue.
- `agents park --on <ref> "<wait>"`: records a parked outcome and retains the
  existing wake route and delivery lifecycle.

`delivery --stats` and `sim observe` separate merged, no-change, blocked,
parked, stalled and dropped outcomes. Every terminal `delivery.step` event
(`done`, `dropped`, `blocked`, `stalled`) records `outcome`, `evidence` and
`outcomeAt`, including lane release verdicts, absorbed or excluded pull
request seeds, abandoned ended-worker deliveries, closed issues or pull
requests, and manual `delivery drop` commands. Unexplained legacy terminal
events are counted as dropped, never inferred as merged. An independent
reviewer reports its review without claiming that the implementation delivery
is complete.

An unfinished headless turn receives its task again, with the valid endings,
up to three times per assignment. When the third continuation also ends
without an outcome, the delivery becomes stalled immediately. Its serving
record ends, capacity is released through normal retirement, and the next
worker's brief includes the last report. A park, an outstanding wake, a role
session or an active turn is not an unfinished-turn recovery candidate.

Release acceptance requires two days of installed observation inspecting both
`delivery --stats` and the raw `delivery.step` events: retain the installed
version and observation window, outcome counts, proof that no terminal
`delivery.step` event lacks an outcome, and the share of started issue
deliveries ending without a pull request, grouped by their recorded reasons.
Passing synthetic tests alone does not establish this live acceptance.

## Repository observations during refill

Conditional GitHub reads reuse the cached value on HTTP 304, including when
`gh` reports exit status 1 for that response. An unchanged pull inventory
refreshes its observation time without listing all open pulls again; the
periodic full refresh still applies.

If a repository's pull observation fails, refill defers that board item with
its normal retry backoff and continues looking for work in other repositories.
It does not start the affected item using stale data. Global GitHub budget
holds, cancellation and capacity admission still stop the refill.

## Idle inventory and GraphQL budget

Pull inventory observations are shared by repository for five minutes. A failed
repository observation is cached for the same interval: idle passes return that
failure without repeating the inventory request or its two fresh GraphQL budget
probes. Starts still require a successful observation; old pulls are retained
only so a later successful conditional read can recover without a full fetch.
Admission holds and canceled passes are not cached as repository failures.

The inventory cost regression simulates an hour of 30-second idle passes with
three repositories. It bounds the inventory path to zero GraphQL calls between
refreshes and at most nine calls on a refresh pass, including budget probes,
even when every repository read fails. This bounds inventory retries, not all
worker activity or all loop queries. A release still needs a 24-hour live
observation of budget holds and refill stalls to establish the end-to-end result.
