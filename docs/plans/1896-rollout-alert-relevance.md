# Alert relevance after a release rollout

Issue: #1896. Related: #1890 and #1897.

Status: proposed design; no runtime behavior changes in this PR. The durations
below are initial policy choices for review, not measurements of alert latency.

## Relevance and scope

A firing alert is relevant when it belongs to the configured team at any hour,
or when it newly fires on an installation during a promoted release's rollout
observation window, regardless of the alert's or release's team. Explicit silences
always win. Initially keep the existing conservative rule of dropping every
silenced alert, since silence ownership is not available in the current snapshot.
Keep ordinary inhibition filtering; extend the working-hours-only exception to
rollout candidates so overnight cross-team regressions are visible too. Apply
this before constructing the durable alert snapshot; changing only `teamAlert`
would miss candidates already removed by normalization.

The scope is the installation, including its workload clusters. Timing establishes
a possible regression, not causation. Do not claim that the release caused an
alert or automatically roll back, silence, promote, or upgrade anything.

## Learning rollouts

Make watch the sole external reader. Add a durable rollout ledger consumed locally
by the supervisor alongside `watch.alert-state`. Each record contains installation
ID, release repository/component, immutable promoted version/digest, target object
UID, source and source event ID, previous revision, apply-start and success times,
observation time, timestamp provenance and status. Persist deduplication keys and
source cursors atomically with records; restarts must not restart windows.

Two adapters feed the same ledger:

- CI auto-releases publish authenticated deployment receipts to a configured
  durable feed after the installation deployment starts and when it succeeds or
  fails. A receipt identifies the actual installation, promoted digest and
  deployment operation. Workflow completion or release publication alone proves
  neither target installation nor rollout. Poll the feed with a persisted cursor;
  validate producer, target allowlist and promotion evidence. This feed is a new
  integration contract, not an assumed existing CI capability. Until supplied,
  report CI coverage unavailable rather than infer success from a green build.
- Flux: list HelmReleases in explicitly configured release namespaces on each
  monitored installation. Compare object UID and successful deployment history
  revisions, using the deployed version and deployment timestamp, as the existing
  `helmrelease-ready` timer does. Also retain an observed transition to a new
  attempted revision and its start time, even if Ready never becomes true. Join
  versions to a cached promotion manifest; an unknown or unpromoted version does
  not open this relevance exception. Do not use an empty applied-revision field
  or a Ready condition timestamp as proof of a new deployment.

Use a stable installation/release/digest/operation identity to join receipts and
Flux evidence for one rollout, keeping both provenances. Never extend a window
merely because another source observed the same rollout later. A rollback or
redeployment is a distinct operation, even when it restores an old digest.
Discover historical successes on first start only within the active window;
baseline older objects without announcing them as new. Keep explicit coverage
gaps for missed history, unknown promotion, inaccessible installations and feeds.
An installation without configured credentials cannot be monitored implicitly.

## Window and attribution

Observe from the first trustworthy apply-start through 60 minutes after success.
If start is unavailable, begin at the successful deployment timestamp and mark
coverage during apply unknown. An attempted rollout without success expires two
hours after start and reports rollout status unknown/failed separately. These
bounds prevent a stuck rollout from making unrelated alerts relevant forever.
An actual later success opens its own 60-minute tail. Delayed ingestion preserves
source times, not ingestion time; expired events never open a fresh hour.

Persist the last successful pre-rollout alert baseline, including fingerprint and
firing start, plus observation timestamp. A candidate must be currently firing,
unsilenced, and have `startsAt` within the inclusive start and exclusive end of
the window. A fingerprint already firing before start is pre-existing noise and
does not become a candidate just because it was first read after the rollout.
A confirmed resolution and new firing start can create a new episode of the same
fingerprint. Keep episode identity separate from the current display baseline,
which may preserve the old start for flap suppression.

With no baseline, a valid start inside the window is still a candidate, labelled
as lacking a baseline. Missing, malformed, future or contradictory timestamps
produce an uncertainty count, not an attributed alert. Do not compensate for
clock skew by silently broadening the window. A read failure is not resolution;
held alerts cannot create new episodes. Late reads can report still-firing
episodes whose starts were in the window, but cannot reconstruct alerts that
already resolved while monitoring was unavailable. Expose that gap.

Overlapping windows list every plausible rollout against one alert episode.
Do not assign exclusive blame, duplicate notes, or repeatedly count the same
episode because a receipt was replayed. Once the window ends, stop admitting new
episodes; retain existing candidates until resolution or explicit acknowledgement
so an unresolved possible regression does not disappear at minute 60. Normal
team relevance continues independently of rollout timing.

## Loop action and decision owner

Default to one local summary line per changed installation candidate set, with
rollout identifiers, counts by severity, oldest new firing, pre-existing and
uncertain counts, and coverage status. No per-alert notes for an unowned burst.
The first new candidate summary wakes the supervisor; unchanged passes and
replayed events do not. Resolutions update the summary and durable candidate set.

Preserve existing owner/park routing first: a waiting worker may be resumed for
its original task. Create a deduplicated decision note only if an unroutable alert
explicitly blocks owned work, following #1890. Include rollout evidence as context
on that note, with one installation/alert-episode identity across overlapping
rollouts. The guide/person decides whether to staff a new investigation; temporal
relevance alone grants no permission to start a worker. Existing capacity and
stop gates still apply. Detailed installation evidence stays local; public issue
or PR text must omit private endpoints and personal data.

## Read budget

Poll rollout discovery every five minutes, shared by every loop consumer. For
`I` accessible installations and `N` configured namespaces per installation,
Flux costs `I * N` logical LISTs per poll, or `12 * I * N` per hour. Pagination
adds one request per extra page. A shared promotion manifest and CI receipt feed
each cost at most one logical conditional read per poll (24 per hour combined),
plus pagination. Cache unchanged versions and cursor results; no per-alert,
per-release, per-worker or per-supervisor-pass GitHub lookups. Back off failures
and honor rate limits; report stale coverage instead of retrying in a tight loop.

Reuse watch's existing Alertmanager readings for monitored installations. Each
additional installation admitted by the configured rollout scope adds one logical
alert query per alert poll while its window or unresolved candidates remain;
transport discovery, fallback and retries can add physical requests. Do not add
per-candidate queries. Publish logical reads, actual requests and last successful
source times so the budget is measurable. A five-minute discovery poll means up
to five minutes of discovery latency plus the next alert poll; preserve any
intervening firing starts from Alertmanager rather than using first-seen time.

## Implementation acceptance tests

Use fake clocks, recorded source responses and request counters; no live cluster
or CI calls. These are requirements for the subsequent implementation:

| Scenario | Required result |
| --- | --- |
| CI start/success receipts and matching Flux revision, replayed after restart | One rollout, original times, no duplicate summary or extended window |
| Flux upgrade with verified promotion, including another team's release | Same installation relevance as CI; unrelated installations unaffected |
| Green CI build without deployment receipt; unknown promotion | No inferred rollout; coverage gap reported |
| Attempt never Ready; success observed late | Bounded attempt window; success tail uses source time |
| Old success at bootstrap; new success within the last hour | Old revision baselined; only still-active window admitted |
| Cross-team firing at start, just before end, and exactly at end | First two admitted; end excluded |
| Old firing first discovered after rollout; confirmed refire | Old episode excluded; genuinely new episode admitted |
| Missing baseline, bad timestamps, failed reads, held alerts | Explicit uncertainty/coverage; no fabricated resolution or attribution |
| Silenced alert; working-hours-only inhibition overnight | Silence excluded; otherwise eligible rollout candidate included |
| Many unowned candidates and overlapping rollouts | One changed summary per installation, no per-alert notes or automatic workers |
| Explicit blocked work; existing parked owner; loop restart | Deduplicated note or routed wake using existing gates |
| Window expiry with unresolved candidate; later resolution | Candidate retained then cleared; no newly starting cross-team candidates admitted |
| Repeated loop passes, pagination and source failures | No loop API reads; measured polling budget and bounded retries |

Before enabling this policy, validate the CI receipt contract and Flux timestamp
and history behavior with sanitized fixtures from both rollout paths. Expose
missing coverage until those adapter contracts are verified. Implement ledger and
adapters first, episode classification second, and summary/routing last; the
design review decides whether the proposed windows and default action are suitable.
