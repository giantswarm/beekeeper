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
releases keep their place. `delivery.step` events retain each transition,
including its original start time, for cycle evidence and statistics.

## Limit and capacity

Set `delivery.wipLimit` with the component's configuration command:

```sh
beekeeper config set delivery.wipLimit 2
```

Zero disables the limit while keeping observation and statistics active.
Capacity still controls running sessions through its existing resource and
subscription constraints. Both must permit an ordinary new issue to start.
A parked worker frees session capacity but keeps its delivery place; a
merge alone also keeps it. An already over-limit desk drains existing work
before admitting another ordinary issue.

The gate covers automatic fill, next-after-done, monitored issues, board
selection and claims, and the final dispatch. Claims count under the state
lock, so simultaneous starts cannot each spend the last place. A full limit
leaves slots empty instead of reading more ordinary board work. A stale
observation also holds new work until a complete loop observation returns.

Reviews, fixes, wakes and merges continue at the limit. Pending reviewers
and fixes without a live worker reserve capacity before ordinary new work.
Manual starts, the named repair of an active incident, and `board.order`
steps with `wipExempt: true` can exceed the limit, but still count. This is
why the count can exceed the configured value without an admission failure.

Stalled work remains counted and is raised after `delivery.stallAfter`.
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
