# Execution retirement proof

The host records creation intents in `resources/<execution>.json`, bound to the
host, execution and generation. Source transfer records its intent before the
first chunk; preparation does so before cloning. Unrecorded existing paths are
not adopted. Shared source checkouts and provisioned toolchains are excluded.

Harnesses receive an execution-owned temporary directory, Go module/build caches
and download GOPATH, and XDG cache directory. These are recorded before creation.
The inventory also names the isolated worktree, local worker branch repository,
scratch, source bundle, GitHub dispatch handles and MCP home.

Stop and terminal inspection use the same retirement operation. Inspection is
also the retry entry point after a host or coordinator restart. An active or
unknown supervisor state prevents retirement, even after a completion report;
parked workers keep their thread and worktree. An interrupted preparation is
eligible only when failed or explicitly stopped. Legacy unrecorded resources
require recovery rather than automatic adoption.

Before moving runtime resources, the host syncs a recovery reference containing
the journal and retained resource locations. Worktrees, dirty/untracked files,
unpublished commits, scratch and source bundles stay at their original paths.
Publication, review, merge and thread evidence is not removed. This preserves the
unpublished-work contract from #2251. Runtime resources are renamed alongside
their original locations with a `.retired` suffix and are never hard deleted by
this proof. No configured retention period is shortened: this slice introduces
no expiry or purge operation.

The inventory records revocation progress separately from process termination.
A failed GitHub callback leaves retirement unfinished with a retryable error;
repeated stop or inspection retries it. Existing coordinator revocation remains
independent of host reachability and retains failed credential revocations in
its credential ledger. Process termination acknowledgement alone is not cleanup
success. If the supervisor cannot establish inactivity, no resources move.

The focused tests exercise stop, failed preparation, inactive/crashed units,
interruption after a rename, dirty work, parked workers, failed revocation and
retry, foreign generations, symlink resources and two isolated workers using one
provisioned repository. The existing unpublished-work preservation test continues
to assert that stop and terminal inspection keep commits and dirty files intact.
