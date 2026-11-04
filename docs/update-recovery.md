# Update recovery protocol

Both latest and pinned installs serialize on the resolved executable's
`<executable>.update/lock`. Executable symlinks remain intact; recovery files,
locks and directories must not be symlinks. The private recovery directory
contains a schema-fenced journal and copied, digest-addressed executables.
Only binaries verified by the release installer enter this store. This is a
local integrity record, not a replacement for release signature verification
or the trusted release evidence gate tracked in #2257.

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

After observing successful operation, run `beekeeper self-update
--complete-probation`. This explicitly ends probation, repeats the existing
local canary checks, and promotes only the journal's matching installed
candidate. A failed health check quarantines the candidate; it cannot be
promoted. Another install is refused while probation or recovery is unfinished.
No elapsed timeout or plugin sync alone counts as successful probation.

The first protected installation cannot authenticate a pre-existing executable's
provenance. Its digest and version are recorded as displaced, but it is not
advertised as a verified recovery target. Complete probation to establish the
first last-known-good. If that first candidate fails, use a compatible signed
release through operator recovery; there is no automatic bootstrap rollback.
Existing pre-protocol executables do not gain protection retroactively.

Request recovery with `beekeeper self-update --rollback --rollback-reason
"failed health check"`. A modified backup is refused. No download is needed.
The live state lock is held from compatibility validation through restoration.
Stable recovery version >= the live state's stable writer version is supported;
a downgrade after the candidate has advanced the writer is refused. A missing
state file is compatible; existing unversioned state and prerelease/unknown
writers require explicit migration. Prerelease candidates cannot be automatically
rolled back because their writes do not participate in stable version fencing.

Probation does not claim backward-compatible state writes. Candidates continue
to use the normal writer version fence. If a candidate advances state, recovery
requires a lossless migration of the **current** state or a signed release that
can read and write it. No such migration is inferred from a lower version
number, and this change does not provide one. Never copy back an old whole-state
snapshot: work, approvals, notes, unknown fields and post-install events must
survive. Recovery changes no state or event bytes and does not lower the writer
stamp. Events remain appendable after recovery.

Before binary rollback, a durable state writer ceiling prevents surviving newer
candidate processes (and unversioned builds) from writing state after the lock
is released. It survives process restart and interrupted rollback. Explicit
probation completion of a compatible installed stable release removes that
ceiling under the state lock. Failure to complete leaves it in place. Direct
side-file and event logging do not advance the state writer stamp.

Normal update restart behavior remains: watch/reopen and gate/merge-child move
to the installed executable at their existing quiet boundaries; alerts and guide
watches require their owner to re-arm them. Rollback does not immediately kill
or restart processes or the broker. Restart candidate processes at a quiet
moment and manually re-arm alerts/guide watches; the state ceiling protects the
interval. Pre-protocol processes that ignore this ceiling must be stopped before
recovery. A caller with multiple state directories must coordinate recovery for
all of them; the CLI guards the configured state directory.

Installation requires write access to both the executable directory and its
recovery directory. The former privileged rename fallback is intentionally
removed because it bypassed the durable transaction. Permission, disk-full,
unknown journal schema and validation failures fail closed. An error after the
executable rename can mean the candidate is already installed: inspect/reconcile
the journal rather than assuming every error left the executable unchanged.
Use a local filesystem providing atomic rename and file/directory sync; this
protocol does not claim durable installation on filesystems without them.
