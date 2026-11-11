# Supervisor state-lock proof

Delivery ownership classification and delivery event derivation run outside the
exclusive state lock. The commit compares the delivery records with the snapshot
used for derivation. A changed delivery set causes a fresh read and derivation,
with at most three attempts per observation; persistent conflicts leave the
observation incomplete for the next pass. Agent, timer and note updates are
preserved without requiring recomputation. Release probes remain outside the
lock, after their attempt reservation has been committed.

## Installed-release acceptance

For the delivery-load regression, define the maximum acceptable supervisor hold
as **3 seconds**, before collecting or assessing evidence. Use an installed,
versioned release containing the fix, with at least **100 deliveries and 20
sessions** throughout the capture window. Retain workload counts at both ends
and identify whether the sessions are running, parked or historical. CI's
synthetic workload is regression coverage, not installed-release acceptance.

Set `BEEKEEPER_STATE_LOCK_TRACE` to an absolute path in a private diagnostic
directory for the supervisor process. The optional JSONL trace records every
completed shared and exclusive state-lock hold from that process:

- `pid`: process that acquired the lock;
- `acquired` and `released`: UTC-compatible timestamps;
- `shared`: whether this was a shared read lock;
- `hold_ns`: elapsed nanoseconds measured with the monotonic clock.

Trace writes happen after unlocking and contain no command arguments, state
contents or local paths. A new trace file has mode 0600. Capture is best effort:
a missing/unwritable trace is not a pass. Use a fresh regular file for a bounded
capture window, then remove the environment setting; tracing has no rotation.
Do not use a pipe or other blocking destination.

Capture several complete supervisor passes while concurrent starts, state
writers and due timers run. Retain the installed version and commit, supervisor
PID and process lifetime, start/end timestamps, raw trace, workload counts,
maximum shared and exclusive hold, and timestamped outcomes of each concurrent
operation. Inspect the holder record at the boundaries: a hold still in progress
has no release row and must not disappear from the assessment. A process exit,
restart, unreadable trace, incomplete pass or missing timing interval leaves
that interval unverified. Correlate timer due/fired times and start/write
completion times with the trace, rather than inferring responsiveness from a
short maximum alone. Collect no state contents or session names in public
proof artifacts.

Do not close the delivery-load issue until this installed-release evidence
passes the threshold and the concurrent-operation checks. Missing proof remains
an open acceptance item even if CI passes and the implementation is merged.
