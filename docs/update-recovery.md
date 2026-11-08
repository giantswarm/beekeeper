# Update recovery protocol

Both latest and pinned installs serialize on the resolved executable's
`<executable>.update/lock`. Executable symlinks remain intact; recovery files,
locks and directories must not be symlinks. The private recovery directory
contains a schema-fenced journal and copied, digest-addressed executables.
Only binaries verified by the release installer enter this store. This is a
local integrity record, not a replacement for release signature verification
or the trusted release evidence gate tracked in
https://github.com/giantswarm/beekeeper/issues/2257.

The journal records the displaced version and digest, previous verified recovery
target, candidate version and digest, UTC installation time, probation phase,
and durable quarantine reasons. Writes sync their temporary file before rename
and sync the containing directory afterwards. Replacement is on the same
filesystem. A crash may leave an unreferenced artifact, never a journal pointing
to an incomplete artifact. Promotion collects only unreferenced digest files,
preserving both the candidate and the active previous recovery target.

| Durable phase | Installed bytes | Recovery action |
| --- | --- | --- |
| No journal | Bootstrap executable | Retry verified installation |
| Prepared | Displaced digest | Retry the identical verified candidate |
| Prepared | Candidate digest | Reconcile to pending probation |
| Pending | Candidate digest | Complete probation or request guarded rollback |
| Passed | Candidate digest | Eligible as the next install's recovery target |
| Rolling back | Candidate or previous digest | Retry rollback with live state guard |
| Rolled back | Previous digest | Idempotent success; failed candidate stays quarantined |
| Any phase | Unexpected digest or modified recovery bytes | Refuse without replacing the executable |

## Automatic recovery

An update launches a recovery controller before it replaces the binary, so a
candidate that never starts cannot prevent it. The controller runs the retained
last-known-good bytes in `beekeeper-recovery.service`, a unit of its own: neither
the candidate's death nor a restart of the loop's unit reaches it. Where no
launcher serves it runs in a session of its own. It needs a verified recovery
target and an updater that is that target, so the first install of this protocol
has none, and such a candidate falls back to the unsupervised rules below.

Every minute it evaluates the candidate, and every decision comes from the
journal, so a killed controller resumes where it stopped. A standby watch of
either binary starts it again after a reboot. `Elapsed` counts evaluations, not
wall-clock time: a machine that slept proves nothing and fails nothing meanwhile.

| Signal | Verdict |
| --- | --- |
| Three consecutive same-reason merge or start failures of this candidate | Roll back at once |
| Three consecutive failed isolated workflow probes | Roll back at once |
| A machine whose loop ran lately, no heartbeat from the candidate's loop 10 minutes after that loop started (the old loop hands over only at its next quiet pass; one that never hands over is judged at the window's end) | Roll back |
| A stop | Nothing counts |
| GitHub unavailable, or heartbeat, outcome or state evidence unreadable | Stays unproven: no rollback, no promotion |
| 30 observed minutes, a current passed probe, the heartbeat where one is owed | Promote to last known good |

The probe needs no backlog, account or network, so an idle desk is judged too. It
drives the candidate through real `supervisor loop --once` passes against a scratch
origin and a fixture GitHub played by the controller's bytes, and proves the merge
in the origin. It repeats every five minutes after a pass, and at once after a
failure or inconclusive run. A probe that cannot be built is inconclusive and
clears the current pass while retaining prior pass history, so a candidate cannot
promote on a stale probe pass. A failed or timed-out candidate probe is repeated on
the last known good as a control: what that cannot do either is the machine's fault,
and counts against no candidate.

A rollback quarantines the candidate by digest and skips its version, so ordinary
self-update offers it no more, and a coordinator refuses to pin a quarantined
release on its execution hosts. One line for the guide names the failed and the
restored version and the reason, retried until it is out, and only once. Running
processes keep their inode, so the replacement harms none of them. They move at
their quiet moments as after an update; a broker serving calls is left to finish; a
service that is down starts at once. A loop still on the failed binary after ten
evaluations is restarted, because a broken loop may never reach a quiet moment.

The loop writes `supervisor-loop-lifecycle.json` after acquiring its exclusive
loop lock and at pass boundaries. It retains the candidate version, startup time,
last phase, and an exit code with a bounded error classification. It contains no
command output or configuration. The missing-heartbeat rollback reason includes
this evidence only when its version and timestamps match the installed candidate.
A startup marker or a stalled pass never counts as a heartbeat, extends the grace
period, or permits promotion. A competing loop cannot overwrite the owner's record.

For a failed rollout, collect the installation and recovery evaluation timestamps,
the loop service's result and exit status, its bounded startup/exit journal, and
the lifecycle record. Compare the last completed-pass heartbeat with the first
candidate startup and phase timestamps to distinguish an incumbent that never
re-executed, a startup/lock failure, an exited pass and a pass still in progress.
Do not run a second loop against live state to obtain this evidence.

The recovery controller runs retained last-known-good bytes. A controller predating
these diagnostics still prints its older rollback reason even when the candidate
writes a lifecycle record. Inspect that record separately during the first rollout;
new rollback formatting is available after a release containing it has become the
verified recovery target. This does not authorize replacing a controller or clearing
a fence to make probation succeed.

An update waits for the verdict of a supervised candidate. A later release can
install after a rollback, and the writer fence lets it write: see below.

A recovery that cannot complete (a missing or modified recovery binary, state the
target cannot write, a rollback failing three times) replaces nothing and destroys
nothing. It reports `RELEASE RECOVERY FAILED`, and holds updates and worker starts.
Deal with the cause, then run `beekeeper self-update --clear-recovery-hold`. The
quarantine stays. A recovery target that no longer verifies is dropped, so the next
install has none, as a first install has.

Known limits: a reboot during probation leaves the candidate unsupervised until a
watch runs, so a machine whose watch is broken by that same candidate stays pending
until an operator acts. An execution host whose coordinator rolled back waits for
its own controller's verdict before it can follow, at most the probation's length.

## Unsupervised candidates

Without a controller, probation ends by itself at the next update, on every machine
and execution host. An update run by the installed candidate is the evidence that it works:
`beekeeper self-update` also repeats the local canary checks on it first, while
an execution host's update, run over SSH by the candidate, has only that evidence.
It then promotes the candidate, which becomes the recovery target. A failed check
quarantines the candidate instead: it is never promoted, the update moves past it
to the new verified release, and the recovery target stays the last known good.

A process of another version proves nothing about the candidate, so its update is
refused while probation is pending; run the installed binary instead.
`beekeeper self-update --complete-probation` ends probation at once, with the same
checks. No elapsed timeout or plugin sync alone counts as successful probation.

A recovery fence is lifted by `--complete-probation` or a controller's promotion; an
unsupervised update's promotion leaves it in place.

The first protected installation cannot authenticate a pre-existing executable's
provenance. Its digest and version are recorded as displaced, but it is not
advertised as a verified recovery target. Complete probation to establish the
first last-known-good; the next update does so too. If that first candidate fails, use a compatible signed
release through operator recovery; there is no automatic bootstrap rollback.
Existing pre-protocol executables do not gain protection retroactively.

Request recovery with `beekeeper self-update --rollback --rollback-reason
"failed health check"`. A modified backup is refused. No download is needed.
The live state lock is held from compatibility validation through restoration.
A recovery version >= the live state's stable writer version is supported, a
release or a prerelease; a downgrade after the candidate has advanced the writer is
refused. A missing state file is compatible; existing unversioned state requires
explicit migration for a stable recovery version, and an unknown writer always does.
A prerelease neither stamps the state nor is judged by the stamp, so a release
candidate cannot have advanced the writer: it rolls back to its predecessor, the
case the desks run. Its state compatibility rests on unknown fields surviving a
save; a prerelease that changes the meaning of an existing field is not detected.

Probation does not claim backward-compatible state writes. Candidates continue
to use the normal writer version fence. If a candidate advances state, recovery
requires a lossless migration of the **current** state or a signed release that
can read and write it. No such migration is inferred from a lower version
number, and this change does not provide one. Never copy back an old whole-state
snapshot: work, approvals, notes, unknown fields and post-install events must
survive. Recovery changes no state or event bytes and does not lower the writer
stamp. Events remain appendable after recovery.

Before binary rollback, a durable state writer ceiling prevents surviving candidate
processes (and unversioned builds) from writing state after the lock is released.
It survives process restart and interrupted rollback. It names the failed release
too: a release past that one is a later release, not a survivor, and writes, or it
could never pass its own probation. Probation completion of a compatible installed
release removes the ceiling under the state lock. Failure to complete leaves it in
place. Direct side-file and event logging do not advance the state writer stamp.

Normal update restart behavior remains: watch/reopen and gate/merge-child move
to the installed executable at their existing quiet boundaries; alerts and guide
watches require their owner to re-arm them. A manual `--rollback` restarts nothing:
restart candidate processes at a quiet moment and re-arm alerts/guide watches; the
state ceiling protects the interval. A controller's rollback restarts as described
above, and alerts and guide watches still wait for their owner. Pre-protocol
processes that ignore this ceiling must be stopped before recovery. A caller with
multiple state directories must coordinate recovery for all of them; the CLI guards
the configured state directory.

An executable directory this account cannot write, a root-owned installation,
keeps its transaction. Its recovery directory moves to
`$XDG_STATE_HOME/beekeeper/update/<hash of the path>` (default `~/.local/state`) and
staging with it. Download, verification, canary checks and the journal run as this
account, and the journal is durable first; only the atomic replacement, of an
update or a rollback, takes `sudo -n`. An interruption around it reconciles like
any other. If sudo refuses, the binary stays, and the journal waits for that same
release to be retried. A journal keeps its place if directory permissions change later.

Disk-full,
unknown journal schema and validation failures fail closed. An error after the
executable rename can mean the candidate is already installed: inspect/reconcile
the journal rather than assuming every error left the executable unchanged.
Use a local filesystem providing atomic rename and file/directory sync; this
protocol does not claim durable installation on filesystems without them.

## Operator runbook: evidence inspection, quarantine recovery, and proof artifacts

- **Inspecting release canary evidence:** Each release publishes
  `<binary>.canary.json` and `<binary>.canary.json.bundle`. Inspect the payload
  with `gh release download <tag> -p '*.canary.json*' -O -` or run a local staged
  verification with `beekeeper self-update --stage-canary <path> --canary-recorded`.
- **Inspecting and clearing local quarantine:**
  - Pre-install canary failures are stored by candidate SHA-256 digest under
    `${XDG_STATE_HOME:-$HOME/.local/state}/beekeeper/canary-quarantine/<sha256>.json`.
  - Post-install probation rollbacks record the quarantined digest in
    `<executable>.update/journal.json` (`quarantine` map) and the skipped version
    tag in `${XDG_STATE_HOME:-$HOME/.local/state}/beekeeper/skipped-releases.json`.
  - Prefer publishing a new fixed release tag rather than un-quarantining a bad
    digest. If a recovery hold (`RELEASE RECOVERY FAILED`) blocked updates and
    starts, resolve the underlying cause and run
    `beekeeper self-update --clear-recovery-hold`.
- **End-to-end containment and recovery proofs (`make test-loop-workflow`):**
  Running `BEEKEEPER_TEST_LOOP_ARTIFACTS=/tmp/loop-workflow-artifacts make test-loop-workflow`
  executes the full integration suite and writes sanitized proof artifacts under
  `/tmp/loop-workflow-artifacts`:
  - `recovery-proof/containment-proof.json`: staged canary evidence for a good
    candidate (`status: "success"`, `merge_commit` populated) alongside failed
    evidence for candidates with a pull-request parser regression and missing
    merge-worktree validation (`status: "failed"`, `merge_commit` empty).
  - `recovery-proof/recovery-proof.json`: probation rollback proof showing the
    retained last-known-good controller detecting consecutive isolated workflow
    probe failures on the broken candidate, restoring the verified previous
    binary within the 30-minute probation window, quarantining the broken digest,
    recording `RELEASE ROLLED BACK`, and passing the post-rollback workflow merge
    probe.
