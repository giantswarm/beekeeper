# SSH execution hosts for agents

Design proposal. Refs https://github.com/giantswarm/beekeeper/issues/1711.
Commands, configuration and types below are proposed, not available commands.

## Scope and existing boundaries

An execution host is where a harness and its workspace run. The coordinating
beekeeper instance remains the owner of scheduling, roster identity, holds,
questions, reports, parks, resumes and subscription capacity. SSH is a transport
to a component command on that host; a wrapper must not implement a second
scheduler, launcher, inbox or approval store.

Today `cmd/agentstart.go` selects Claude Code or omp with branches, rather than
a shared Go harness interface. `cmd/agentstartomp.go` and
`cmd/agentbackground.go` own their launch details; `internal/platform.Launcher`
owns user services. `recordStart` registers a start before delivering its task.
`registerAgent` can replace a same-name entry when local discovery finds no
process. That last rule cannot establish that a remote worker has stopped.

`cmd/agentwake.go` dispatches messages by harness, including the agy inbox from
https://github.com/giantswarm/beekeeper/pull/1701. That merged change is a message
runner, not a complete agy start implementation. The Codex harness in
https://github.com/giantswarm/beekeeper/issues/1708 is also still a dependency,
not an assumed capability. The first remote adapter uses Claude Code with a
headless first turn and an explicit conversation identifier; it uses neither a
desktop import nor a shared harness daemon.

The agy guards and skills from
https://github.com/giantswarm/beekeeper/pull/1698 and
https://github.com/giantswarm/beekeeper/pull/1705 establish checks and worker
routes that must also run on an execution host. Provider counting from
https://github.com/giantswarm/beekeeper/pull/1703 already reads roster harness
and model metadata. Extend that counting, rather than assigning a new budget
to every host. These changes continue
https://github.com/giantswarm/beekeeper/issues/1682,
https://github.com/giantswarm/beekeeper/issues/1683,
https://github.com/giantswarm/beekeeper/issues/1685 and
https://github.com/giantswarm/beekeeper/issues/1684.

## Selector, configuration and commands

Keep the existing local start path when `--host` is absent or `local`. Never
fall back to local execution when a named remote host fails. Initially select
a remote host explicitly; `--harness auto` still chooses a provider/model,
not a machine. Unknown hosts and unsupported host/harness combinations fail
before reserving capacity or creating a workspace. `--force` cannot bypass
identity, version, policy, ownership or reachability checks.

Add `agents.hosts` to `internal/config`, with a reserved `local` name:

```yaml
agents:
  hosts:
    host-a:
      ssh: host-a.example
      identity: host-a
      executable: /usr/local/bin/beekeeper
      config: /etc/beekeeper/worker.yaml
      workspaceRoot: /srv/beekeeper/workspaces
      repositories:
        giantswarm/beekeeper: /srv/beekeeper/checkouts/beekeeper
      harnesses: [claude]
      policyDigest: sha256:<installed-policy-digest>
      maxBusy: 2
      connectTimeout: 10s
      operationTimeout: 30s
```

`ssh` is a single SSH configuration alias or destination, never a command or
an option; reject whitespace, control characters and a leading dash. Host
keys must already be trusted. Use batch authentication, strict host-key
checking, no agent forwarding, no TTY and no credential/environment forwarding.
Do not accept extra SSH arguments from a brief. SSH configuration is operator
managed; validate effective security settings and explicitly override unsafe
forwarding or host-key settings. Paths must be absolute, clean host paths;
reject traversal and control characters. Resolve symlinks and containment on
the host too. A destination's reported configured identity must match
`identity`; a changed SSH key or identity fails, never silently rebinds.

Proposed component CLI:

```text
beekeeper agents hosts check <host>
beekeeper agents hosts prepare --bundle <file>       # run on execution host
beekeeper agents start <name> <brief-file> --host <host> --repo <owner/repo> --ref <git-ref> --model <model>
beekeeper agents inspect <agent>
beekeeper agents logs <agent> --after <cursor> --limit <bytes>
beekeeper agents stop <agent> --reason <text>
beekeeper agents recover <agent>
beekeeper agents hosts sync <host>
beekeeper capacity --host <host>
```

`--host`, `--repo` and `--ref` are additions to start. A remote start requires
an allowlisted repository; `--dir` is local-only and is rejected with a remote
host. Its brief file is read by the coordinator and transferred as data, not
looked up on the host. `--dry-run` prints the validated selection and plan,
without provisioning, reserving or launching. `--desktop` and desktop mode
are rejected remotely. A remote start defaults to a new remote-only
`--mode headless`; branch into remote validation before the existing local
mode validator. Require an explicit model remotely. `inspect`, `logs`, `stop`,
`recover` and `hosts sync` are new commands; `wake` retains its existing syntax
and gains remote routing only when its adapter can acknowledge delivery.

`hosts check` is read-only. It checks the remote protocol, configured identity,
beekeeper and harness versions, user service manager, repository mappings,
root permissions, authentication readiness, rules, skills and hooks. Capability
requirements are a versioned protocol manifest, with a tested harness version
range; unknown versions fail closed. A version string alone is insufficient:
run harmless hook conformance probes and require the matching policy digest.
Check authentication readiness without making a paid model call or returning
credential values.

`hosts prepare` is an explicit operator operation before launch: atomically
install a versioned policy bundle and hooks in a managed worker configuration,
preserving unrelated configuration. It does not log in to a provider. Start
only validates this installation; it never edits a user's global rules as a
side effect. Verify the bundle again immediately before executing the harness.
The host's operator-owned configuration pins trusted coordinators, their
verification keys and allowed policy digests. A start request cannot replace
those pins or weaken the host's repository and path restrictions. Hook scope
must cover every managed workspace and tool process, including outside-cwd
paths; an empty or nonmatching scope is a failed check.

## Go contracts and wire format

Add small interfaces in `internal/execution/spec.go`, composed for the SSH
implementation and fake transport. The harness adapter runs on the host and
uses `internal/platform.Launcher`; it does not know about SSH.

```go
type HostID string
type ExecutionID string

// config.ExecutionHost is stored under agents.hosts.
type ExecutionHost struct {
    SSH, Identity, Executable, Config, WorkspaceRoot string
    Repositories map[string]string
    Harnesses []string
    PolicyDigest string
    MaxBusy int
    ConnectTimeout, OperationTimeout Duration
}

type Ref struct {
    Host HostID
    Execution ExecutionID
    Generation uint64
}

type StartRequest struct {
    Ref Ref
    RequestID string
    Coordinator string
    Name, Task, Repository, GitRef, Harness, Model string
    Prompt string
    PolicyDigest string
    RegistrationReceipt string
}

type Handle struct {
    Ref Ref
    Repository, Commit, Workspace, Unit, ConversationID string
}

type Observation struct {
    Handle Handle
    ProcessState string // prepared, running, exited, failed, unknown
    Reachability string // reachable, unreachable, identity-mismatch
    ObservedAt time.Time
    StopRequested, StopAcknowledged bool
    LastError string
}

type Envelope struct {
    ID string
    Sequence uint64
    From, To Ref
    Kind, Text string
}

type Ack struct {
    ID string
    Sequence uint64
    Stage string // stored, accepted, applied
}

type StopRequest struct { ID, Reason string }
type LogRequest struct { After string; Limit int }
type LogPage struct { Data []byte; Next string; Truncated bool }

type Starter interface {
    Start(context.Context, StartRequest) (Handle, error)
}
type Inspector interface {
    Inspect(context.Context, Ref) (Observation, error)
}
type Messenger interface {
    Message(context.Context, Ref, Envelope) (Ack, error)
}
type Stopper interface {
    Stop(context.Context, Ref, StopRequest) (Ack, error)
}
type Logger interface {
    Logs(context.Context, Ref, LogRequest) (LogPage, error)
}
type Transport interface { Starter; Inspector; Messenger; Stopper; Logger }
```

`SSHTransport` resolves `HostID` only through validated configuration. It runs
one fixed, shell-quoted executable and fixed `agents host-rpc --protocol 1`
arguments, with bounded JSON requests on stdin and JSON responses on stdout.
No prompt, repository, path, identity or message is interpolated into shell
source. Stderr is a bounded diagnostic stream; redact credentials and keep it
separate from the protocol. Reject unknown fields, oversized input, unknown
operations and mismatched response identities. All operations have deadlines;
context cancellation closes the SSH connection, not the remote user service.

The RPC envelope has `protocol`, `requestID`, `operation`, `ref` and a typed
payload. Replies distinguish refusal, unsupported capability, transport loss
and operation failure. A lost reply means unknown outcome, not failed launch.
Read-only negotiation precedes every mutating operation. Add a host-local
`Runner` composed from the same lifecycle interfaces with `Capabilities` and
policy validation. Register adapters explicitly; do not rewrite existing local
launch paths or claim every existing harness implements the new contract.

## Workspace, identity and registration transaction

The coordinator allocates an opaque execution ID and a start request ID before
contacting the host. Persist them with the request digest and capacity
reservation. Retrying the same request returns its existing handle. Reusing
an ID with different content is a refusal. The durable record key includes
coordinator identity and execution host, so another coordinator cannot adopt it.

The digest covers the immutable request fields, including the execution
generation, requested repository/ref, prompt, harness, model and policy. It
excludes the registration/activation receipt, which authorizes the next
handshake phase without changing the request. Pin the resolved commit at
preparation; a retry never resolves a moving ref again. Every operation carries
the expected generation in `Ref`; a stale ref cannot stop or message a later
process generation.

On the host, resolve the repository identity from its own mapping and verify
the checkout's origin against the allowlist. Do not send or reuse local paths.
Create a per-execution isolated clone below `workspaceRoot`, using no shared
writable Git metadata, hardlinks, object alternates or borrowed object stores.
Fetch and resolve the requested ref in that clone, recording its commit. Do
not reset, clean, stash, switch or update the source checkout; its uncommitted
work remains untouched. Preserve failed and stopped workspaces for recovery.
Never turn a caller-supplied execution ID into an unchecked path or unit name.

Startup is a journaled handshake, not a distributed atomic transaction:

1. The coordinator checks holds and shared capacity, reserves the name and
   capacity atomically, and persists a `pending` execution and roster entry.
   Same-name unknown or unreachable executions block replacement. This counts
   as the one registration; later steps update it by execution ID.
2. Host `Start` without a receipt prepares the isolated workspace and durable
   handle under an execution lock, but runs no harness. The adapter reserves
   its conversation ID and deterministic full-length unit identity. Persist
   the request digest before doing work; recovery reuses this preparation.
3. The coordinator commits the handle to that roster entry and issues a
   registration receipt bound to the host, execution, generation and request
   digest. It is verified against preinstalled coordinator trust on the host.
4. Retrying `Start` with that receipt activates the prepared unit exactly once.
   The service checks its journal and the service manager under the same lock.
   Its worker identity and rules are installed before the first task runs.
5. Inspect confirms that the managed runner has accepted the initial prompt
   and recorded its conversation ID. Only then print success with execution
   ID, host, workspace, unit and conversation ID. A task that already exited
   successfully is reported as exited, not falsely running.

Add an optional `Execution *ExecutionRecord` to `state.Start` and a reference
on `state.Agent`. `ExecutionRecord` stores the handle, request digest, phase,
reservation, last observation and stop intent. Do not repurpose
`Party.HostSession`, which means a harness/desktop identifier, into a hostname.
Keep `Party.Host` as the configured execution identity. Absence of execution
metadata preserves existing local state and JSON behavior.

```go
type ExecutionRecord struct {
    Handle execution.Handle
    RequestID, RequestDigest, Phase string
    Provider, Account, Bucket string
    ReservedAt time.Time
    Observation execution.Observation
    StopRequest *execution.StopRequest
}
```

Activation must also confirm that the coordinator's reservation is still
active and no stop or hold intervened. Use a short-lived activation receipt
and a live coordinator check; failure to confirm leaves the unit prepared.
An old registration receipt is never permission to start after a global stop.
Serialize activation and stop against the execution's generation on both
sides. Once activated, the managed runner records prompt acceptance before
running the first model call, making recovery independent of SSH output.

An interrupted prepare, registration write, activation or success reply leaves
a recoverable pending record. `agents recover` inspects both journals, checks
the receipt and resumes the same handshake. If remote absence cannot be
proved, retain the reservation and report unknown outcome. Never start a
replacement. A known preparation failure may release the reservation after
recording the terminal failure; retain its workspace and audit record.

## Worker control and durable delivery

Every managed runner installs `BEEKEEPER_EXECUTION_ID` and a managed worker
configuration reference in the actual service environment. Resolve identity
from the host's execution journal and authenticated channel, not from a text
prefix on a command or an untrusted `--as` value. Clear inherited harness
identity variables. Test through a login shell and through hook subprocesses.

The first slice needs worker control as well as process launch: remote notes,
`agents idle --done --report`, parks and resume outcomes must update the
coordinator. Add a narrow coordinator RPC endpoint and a `CoordinatorClient`
for these operations. Provision the host's callback SSH identity separately,
restricted to that endpoint and its registered host. It cannot start agents,
grant approvals or impersonate the supervisor. No forwarded SSH agent,
provider token, copied credential store or inherited local configuration.

Worker control requests carry execution ID, generation and an idempotency key.
Apply them to the coordinating store transactionally with the result receipt;
a retry returns that receipt, including the original note number. Never fall
back to filing a second local note when an acknowledgement is lost. A failed
callback returns an explicit pending/error result; a durable host outbox keeps
the request. `hosts sync`, also called by the watch, exchanges outstanding
requests and receipts over the coordinator's SSH connection. Unsupported
worker control commands refuse explicitly, rather than changing host-local
scheduling state. The managed config routes worker control before the existing
optional central-instance logic; do not publish a second roster entry through
`syncRoster`.

The execution host's managed configuration adds
`agents.coordinator.{identity,ssh,executable,config}` for this callback; all
four are provisioned there, never copied from a caller's environment. The
coordinator's SSH authorized-key restriction binds the connecting key to one
configured host. Check the execution's host, generation and permitted operation
against its journal before applying a request; reject supervisor impersonation.
Control envelopes use the same persistent sequence/deduplication mechanism as
messages, so an old report cannot overtake a newer park or assignment.

The full message path uses an outbox at the sender and inbox at the receiver,
with monotonic sequences per execution and direction. Persist before replying
`stored`; `accepted` means the harness recorded that message, and `applied`
means a coordinator mutation committed. SSH exit zero alone is none of these.
Acknowledgements and deduplication cursors survive restarts; retain the
acknowledged high-water mark for the execution's lifetime. A gap is held until
the missing sequence arrives; a repeated ID with different content is refused.
The initial prompt is a reserved message and is not sent twice on recovery.

Reuse the envelope and acknowledgement concepts from `internal/mailbox` and
the delivery integration in `cmd/centralmailbox.go`, without requiring a
Postgres-backed central deployment for a local coordinator. Its current
time-limited deduplication and acknowledge-after-wake behavior do not prove
remote harness acceptance; adapt that boundary explicitly rather than treating
a successful SSH write as an existing mailbox acknowledgement.

Reconnection exchanges cursors and retries only unacknowledged envelopes.
Never resume or spawn a harness merely because an SSH connection ended.
`agents wake`, note answers, timers and park resumes must all enter this same
path. Preserve the park until its resume is accepted. Show queued, accepted
and delivery-failed states, with retry/recovery commands.

Harness acceptance needs evidence containing the message ID, not a substring
match on message text. The current agy queue cannot be assumed to provide
unbounded deduplication or that evidence. For an adapter without a verifiable
acceptance event, report uncertain delivery and require reconciliation; do not
claim exactly-once model execution. The transport guarantees durable ordered
deduplication; crash-atomic model consumption remains a harness capability.

## Capacity, reachability and stop

The coordinator counts all registered executions, including pending and
unreachable ones, against one provider/account/bucket pool. Provider usage is
an account-level reading, with the existing freshness and pace checks, not a
sum of percentages from hosts on the same subscription. Resolve the pool from
trusted configuration; a remote adapter cannot invent a fresh subscription.
Concurrent starts reserve a slot under the coordinating store lock.
Count a pending reservation and its roster entry once by execution ID, not
as two workers. Terminal, acknowledged executions release their execution
reservation while retaining their unfinished task for explicit reassignment.

Apply target-host machine guards and `maxBusy` separately from subscription
capacity. A busy coordinating machine does not imply a busy execution host.
Stale host resource readings refuse new starts on that host. Preserve global
holds, build-slot admission and stop state, while builds use the execution
host's own `beekeeper run` slots. The initial slice has explicit placement;
automatic placement is separate from provider/model selection.

Task state remains assigned, parked, idle or done. Process state and timestamp
come from the owned remote unit. SSH reachability is a separate observation;
an expired observation becomes unknown, never exited or idle. List this in
`agents`, `inspect`, capacity JSON and the watch. Do not let local discovery,
same-name takeover, stale-roster pruning or desktop repair archive a remote
execution. A human session on the same host is never adopted.

Persist stop intent before sending it. `Stop` verifies execution ownership and
unit generation, records the request, stops only that unit's process group and
acknowledges after observing termination. Repeating a stop is safe. An
unreachable host leaves `stop requested; acknowledgement pending` and returns
nonzero, retaining capacity and task state. `recover`/`hosts sync` retries the
same request. A global stop contacts every owned execution and names each
unacknowledged stop. An independent, expiring coordinator lease in the full
design makes remote runners stop themselves if control is lost; document the
lease duration and that disconnected stop cannot be immediate.

Logs are read from the recorded owned runner/unit, bounded and cursor based.
Reject arbitrary journal units and paths. A disconnect returns the last cursor
and an explicit error; it never restarts the worker. Retain output and
workspace after stop. Archive only an execution this component created, and
only after confirmed termination; archiving a personal conversation is refused.

## Guards and merge authority

The policy bundle includes repository allowlists, protected paths, secret
handling, worker rules, topic skills and the harness-specific hook bindings.
Remote paths are validated there; checks on the coordinator cannot substitute
for enforcement where tool commands execute. Verify unresolved normalized
paths as well as resolved symlink targets, including credential symlinks.
Unknown tools, malformed guard input and internal hook failures deny. Command
hooks inspect interactive writes too, or disable interactive execution.

The full authorization record is `MergeAuthorization{ID, Repository, PR,
ReviewedHead, ExecutionID, Host, Generation, IssuedBy, ExpiresAt, RevokedAt}`.
Only the coordinating supervisor can issue or revoke it after independent
review and CI. Host hooks and the merge component check that exact tuple,
fresh remote head, expiry, revocation and global holds immediately before the
merge, using the coordinator's live authorization service. Consume the grant
atomically when committing its one merge attempt; retry uncertain outcomes by
querying the merge result first. Audit issuance, denial, use and revocation on
both sides with the same authorization ID. A moved head requires a new review
and go. Unreachable authority, missing or stale grants deny; a cached signed
grant alone cannot prove it has not been revoked.

First-slice remote hooks deny every merge and approval-grant operation.
Operators merge from an already approved coordinating workflow until remote
authorization is implemented. No allow-by-default stub and no detached merge
that can outlive its authorization check.

Hooks constrain supported harness tool calls; they are not an OS security
boundary against a user controlling the same account or an unhooked tool.
Preparation must report these limitations and reject a harness whose required
tool surface cannot be guarded. Use the existing sandbox/broker for stronger
isolation where configured; do not advertise hook coverage as sandboxing.
For Codex specifically, a hook error must produce a blocking response rather
than exit 1, and tool/patch/interactive coverage must be proven before that
adapter is enabled. Provider authentication is provisioned independently on
each host; starting a session never copies or forwards provider credentials.

## First implementation slice and acceptance tracking

PR 2 starts after this design is approved and merged. It adds validated host
configuration, the SSH RPC boundary, the Claude Code adapter, isolated
workspaces, the journaled registration/activation handshake, lifecycle commands,
separate reachability, owned stop acknowledgements, baseline worker control
receipts, coordinator reservations and a remote policy that denies merges.
It keeps all existing local start and wake behavior. Tests use a fake
transport/runner and, optionally, a loopback sshd with throwaway keys; never a
real execution host, provider login, paid model call or real merge.

Use `Refs https://github.com/giantswarm/beekeeper/issues/1711` on PR 2 unless
every acceptance line is met. Before requesting its review, file a follow-up
for each deferred line below and link it from PR 2. The parent remains open.

The implementation's [operator guide](ssh-execution-hosts-operations.md) gives
the concrete configuration and commands. The messaging follow-up in
https://github.com/giantswarm/beekeeper/issues/1714 adds an ordered durable outbox,
host inbox, streaming runner and UUID-bearing harness acceptance. Deferred capabilities remain in
https://github.com/giantswarm/beekeeper/issues/1715 (coordinator lease), and
https://github.com/giantswarm/beekeeper/issues/1716 (merge authorization).

| Acceptance line | First slice | Required follow-up if deferred |
| --- | --- | --- |
| Selector and five-operation transport; identity/version/rules validation; local behavior | Selector, lifecycle methods and durable `Message` delivery; local dispatch unchanged | None |
| Per-host checkouts and workspaces; preserve dirty work; isolated Git metadata | Complete for the Claude adapter | None |
| User supervisor; stable handle; one registration; interrupted startup recovery | Complete, including before-activation receipt and recovery command | None |
| Durable ordered acknowledged messaging; identity; notes/reports/parks/resumes; no duplicate process | Durable controls and wake envelopes; ID-bearing harness acceptance, ordered cursors, retained parks and explicit uncertain delivery | None; crash-atomic model execution remains a harness limitation |
| Shared capacity and holds; reachability; stop acknowledgement/unreachable state | Shared provider counts and reservations, host guards, explicit placement and acknowledged stop | Expiring coordinator lease and automatic recovery after a disconnected global stop |
| Matching host rules/hooks and bound revocable audited merge authorization | Validate installed bundle; enforce host guards; deny all remote merges | Supervisor authorization service, grant consumption and host merge guard |
| Separate authentication and ownership restrictions | Complete; no copying credentials or adopting personal sessions | None |
| Failure tests, operator status and recovery | Lifecycle, identity, policy, ownership and refusal cases below | Message acceptance/reconnect, authorization lifecycle and control-lease tests |

## Verification matrix

Tests assert persisted state and observable results across fresh coordinator
and host processes, not just mocked method calls. Inject interruption on both
sides of every durable write and reply. Count harness launches, roster
registrations, capacity reservations and applied control mutations.

| Case | Test and operator result |
| --- | --- |
| Invalid/changed host, key or identity | Fake SSH/loopback key mismatch; fail before clone or launch; `hosts check` identifies the failed check |
| Unsupported harness/version, missing rules/hook, stale digest | Conformance probes and table tests fail closed; preparation is named, no model process starts |
| Injection, hostile refs/paths, oversized JSON, unexpected reply | Transport tests show fixed remote command, data only on stdin, bounded output, no workspace escape |
| Dirty source checkout and isolated workspace | Real temporary repositories, modified/untracked files and symlinks; source work and Git metadata unchanged, workspace independent |
| Startup before roster registration | Pause before coordinator commit; zero harness launches; fail activation with absent, wrong or replayed cross-execution receipt |
| Interrupted startup, duplicate start and coordinator restart | Drop prepare/activate replies and race identical requests; recover same handle, one unit, one roster entry; conflicting payload refused |
| Unit failure or prompt not accepted | No start success; show recorded failure or unknown outcome with `recover` command; preserve workspace |
| Shell identity preservation | Fake harness executes a login shell and hook process, then note/report/park; coordinator attributes each to the original execution; spoofed identity/`--as` refused |
| Lost callback acknowledgement | Retry note/report/park with the same key after restart; one mutation and original receipt; no host-local fallback note |
| Transport interruption during work or logs | Unit continues, task unchanged, reachability unreachable; logs resume at cursor; inspect/recover never launch |
| Duplicate/out-of-order messages and lost acceptance | Full message follow-up restarts both sides around store/accept/ack; one accepted envelope per ID in sequence, uncertain consumption visible |
| Unreachable host during stop | Durable pending stop, nonzero result, capacity retained; reconnect retries same stop; termination acknowledged before freeing reservation |
| Shared subscription and simultaneous starts | Two fake hosts sharing a provider; one usage reading and aggregate busy count; final slot reserved once; stale usage/host metrics and holds refuse |
| Stale/missing authorization | Slice 1 refuses every remote merge; full authorization tests wrong host/execution/repository/PR/head, expiry, revocation, moved head and unreachable authority |
| Personal sessions coexist | Seed unrelated unit/conversation and same-name host entry; inspect/stop/archive never touch them; local discovery cannot replace an unreachable owned worker |
| Local regression and portability | Existing start/wake/park/capacity suites unchanged; full `make test`, lint and platform build checks from `docs/development.md` through `beekeeper run` |

Operator recovery always begins with `agents inspect`, then `agents recover`
or `agents hosts sync`. These reconcile existing identities and pending
requests; none means "try a new start". Status distinguishes preparation,
registration, running/exited/unknown process observations, transport failure,
pending stop and undelivered control. A public report uses only generic
examples; operational identities and workspace paths belong in local state.
