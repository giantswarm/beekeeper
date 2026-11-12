# Supervisor state-lock proof

Delivery ownership classification and delivery event derivation run outside the
exclusive state lock. The commit compares the delivery records with the snapshot
used for derivation. A changed delivery set causes a fresh read and derivation,
with at most three attempts per observation; persistent conflicts leave the
observation incomplete for the next pass. Agent, timer and note updates are
preserved without requiring recomputation. Release probes remain outside the
lock, after their attempt reservation has been committed.

## Transactions and the commit lock

Every transaction holds two locks. `state.lock` serializes the transactions of
all processes. `state.commit.lock` serializes writes of `state.json` and is also
the only lock a remote execution's lease renewal takes; a renewal waits for it
at most 5 seconds and otherwise fails with "coordinator lease state commit busy".
A renewal holds it while it reads, decodes and writes the whole state. A
transaction holds it only for its writes:

- A transaction reads, decodes and copies the state under `state.lock` alone.
  Only a pending event outbox is recovered under the commit lock.
- It encodes the state before taking the commit lock. With events, the staged
  document (the state carrying its event outbox) is the final encoding with the
  outbox appended as its last member, so the state is encoded once, not twice.
- At the commit, a `state.json` that is still the file the transaction read
  means no renewal replaced it: nothing is read again. After a renewal, only the
  writer stamp and the execution leases are decoded and carried forward, and
  only then is the state encoded again under the lock.

Decoding keeps a newer release's unknown members by scanning each object's
member names, copying only the unknown ones, instead of decoding every object a
second time into a map of raw messages.

## Installed-release acceptance

For the delivery-load regression, define the maximum acceptable supervisor hold
of `state.lock` as **3 seconds**, and the maximum acceptable hold of
`state.commit.lock` by any process as **2 seconds** (under half a renewal's
5-second wait), before collecting or assessing evidence. Use an installed,
versioned release containing the fix, with at least **100 deliveries and 20
sessions** throughout the capture window. Retain workload counts at both ends
and identify whether the sessions are running, parked or historical. CI's
synthetic workload is regression coverage, not installed-release acceptance.

Set `BEEKEEPER_STATE_LOCK_TRACE` to an absolute path in a private diagnostic
directory for the supervisor process, and for the CLI processes under
assessment. The optional JSONL trace records every completed shared and
exclusive state-lock hold, every commit-lock hold, and every wait for either
that ended without the lock:

- `pid`: process that waited for or acquired the lock;
- `command`: its subcommand path and the names of the flags given, never an
  argument or a flag's value (`beekeeper supervisor loop --config`);
- `lock`: `commit` for `state.commit.lock`, absent for `state.lock`;
- `shared`: whether this was a shared read lock;
- `busy`: the wait ended without the lock; no hold follows;
- `wait_ns`: elapsed nanoseconds from the request to the acquisition, or to
  giving up;
- `acquired` and `released`: UTC-compatible timestamps, absent on a busy row;
- `hold_ns`: elapsed nanoseconds measured with the monotonic clock.

The exclusive holder's record beside the lock, `state.lock.holder`, and the
lock-busy error name the holder with the same `command`.

Trace writes happen after unlocking and contain no command arguments, flag
values, state contents or local paths. A new trace file has mode 0600. Capture
is best effort: a missing/unwritable trace is not a pass. Use a fresh regular
file for a bounded capture window, then remove the environment setting; tracing
has no rotation. Do not use a pipe or other blocking destination.

Capture several complete supervisor passes while concurrent starts, state
writers, lease renewals and due timers run. Retain the installed version and
commit, supervisor PID and process lifetime, start/end timestamps, raw trace,
workload counts, maximum shared and exclusive hold, maximum commit-lock hold,
busy rows, and timestamped outcomes of each concurrent operation. Inspect the
holder record at the boundaries: a hold still in progress has no release row
and must not disappear from the assessment. A process exit, restart,
unreadable trace, incomplete pass or missing timing interval leaves that
interval unverified. Correlate timer due/fired times and start/write completion
times with the trace, rather than inferring responsiveness from a short maximum
alone. Collect no state contents or session names in public proof artifacts.

Do not close the delivery-load issue until this installed-release evidence
passes the thresholds and the concurrent-operation checks. Missing proof remains
an open acceptance item even if CI passes and the implementation is merged.
