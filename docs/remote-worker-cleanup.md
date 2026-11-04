# Remote worker resource retirement

Each execution owns the directory named by its execution ID beneath the
configured workspace root. Its isolated repository and branch refs never
share writable Git metadata with provisioned sources. Managed harnesses put
the worker home (including `~/.local/state/go-toolchain`), temporary files, Go module/toolchain downloads, Go build caches, Cargo caches
and XDG cache/state files in that execution's `runtime` directory. The agy adapter
uses a separate execution-owned harness home beside its journal. Provisioned
toolchains and harness authentication remain shared host resources.

Stop acknowledges process termination separately from cleanup. The journal's
`cleanup` field records the last cleanup attempt, retryable failure and retained
recovery work. Repeating stop retries cleanup, including authorization revocation.
Inspection reconciles terminal runs through the same stop path. Active services
and parked threads between turns are not retired by inspection.

Retirement removes runtime files, generated GitHub dispatch scripts and disposable
MCP homes. A workspace is removed only when its working tree has no changes,
untracked or ignored files (except the designated scratch directory), and no ref
or reflog contains commits beyond the original pinned commit. Its isolated branch
copies and source bundle are then removed together. Failed or interrupted
preparations and any work that cannot be proved disposable remain in place for
recovery. Recovery work has no automatic expiry; the coordinator/operator owns
publication or recovery before removal. This deliberately preserves the contract
that a stopped worker's unpublished work must remain recoverable.

Execution logs are beekeeper state retained for seven days after their last
write. Terminal cleanup removes older logs once the workspace is proved disposable;
logs accompanying retained work remain recovery evidence. Journals, publication evidence and
review receipts are not expired by this operation. A failed containment check,
symlink, revocation failure or unknown ownership prevents destructive cleanup.
There is no sweep of the home directory or global temporary directories.

`beekeeper agents hosts check <host>` includes a read-only `leftovers` inventory
in its JSON response. It distinguishes active state, retained recovery work,
overdue terminal resources and unknown managed entries. Findings identify the
execution and generation where known, explain the reason, and give the next
action. Checking never invokes retirement or follows unknown symlinks. To retry
a known execution's retirement, repeat its stop; investigate unknown entries
before establishing ownership. Legacy files outside managed roots cannot be
safely attributed to an execution and require operator investigation.
