# Operating SSH execution hosts

Managed, headless Claude Code sessions run on an explicitly selected SSH host.
Each execution gets an isolated clone, an owned user service,
a conversation identifier, and one coordinator roster registration. Omitting
`--host`, or selecting `--host local`, keeps the local launch path.

Read the [design](ssh-execution-hosts.md) for the ownership and failure contract.
Remote wake and resume use durable message receipts as described below. New
executions require expiring coordinator leases and live supervisor merge grants.
The component command below is the supported remote merge path; local approvals
and raw merge commands do not authorize an SSH execution.

## Provision the components independently

Install the same beekeeper version on the coordinator and execution host. The
host installation may sit at any absolute path, including a per-user one; the
pinned configuration and prepared policy bind that exact executable. A managed
worker calls its component by bare `beekeeper`, and the runner places the
bound installation's directory first on the harness PATH, so a system-wide
installation is not required. Install the prepared executable under the name
`beekeeper`: `agents hosts check` refuses a host where the bare command does
not resolve to the very executable the prepared policy binds (missing, not
executable, or shadowed by a stale sibling), because that worker could not
report or park. The host needs Linux with a working user service manager,
Git, and a pinned Claude Code version
in the supported family (`>= 2.1.268, < 3.0.0`), a pinned Codex CLI (see
"Codex on an execution host"), or both. Provision its subscription login
there independently. `agents hosts check` reads version and authentication
metadata; it makes no model request. Starting a session never copies a provider
credential store or forwards an authentication agent.

Provision SSH authentication and known-host keys separately in both directions:
the coordinator calls the host's component, and the managed worker calls the
coordinator's component for controls. SSH runs with batch mode and strict host
key checking, without agent, X11, environment, or port forwarding. A missing key
or changed host key fails the operation rather than prompting. Prefer dedicated
SSH routes restricted to the component RPC commands; the configured coordinator
callback route must carry the execution host identity with `--host`.

All example paths below are operator-owned absolute paths. Source checkouts,
managed policy state, and workspace roots must be disjoint. Managed directories
must not resolve through symlinks. Existing uncommitted source work is preserved;
the component clones and pins a committed ref from the host-local source.

Begin with a coordinator configuration that has `stateDir` and an explicit
`identity.host`, without a host catalog entry yet:

```yaml
stateDir: /srv/coordinator/state
identity:
  host: coordinator
```

Create its signing key once and obtain only the public half:

```sh
beekeeper --config /etc/beekeeper/coordinator.yaml agents hosts key
```

Configure the execution host using that public key. Replace the public-key
placeholder and pin the version actually installed before validating it:

```yaml
stateDir: /srv/beekeeper/state
github:
  allowedOrgs: [example]
agents:
  execution:
    identity: host-a
    workspaceRoot: /srv/beekeeper/workspaces
    rulesFile: /etc/beekeeper/worker-rules.md
    maxBusy: 2
    repositories:
      example/repository: /srv/checkouts/repository
    harnesses:
      claude:
        executable: /usr/local/bin/claude
        version: 2.1.290
        configDir: /srv/beekeeper/claude
    coordinators:
      coordinator:
        publicKey: <coordinator-public-key>
        callback:
          ssh: coordinator.example
          executable: /usr/local/bin/beekeeper
          config: /etc/beekeeper/coordinator.yaml
```

The source checkout's origin must identify `example/repository` on GitHub.
`rulesFile` contains host-specific worker instructions, not authentication
configuration. Known credential files and credential-shaped content are refused.
The prepared policy binds those instructions, embedded worker rules, hooks,
binary version, and host security configuration. A host requiring an OS sandbox
is refused by this adapter; hooks do not provide that sandbox.

Export and inspect the host policy manifest, then install that exact manifest
using component commands on the execution host:

```sh
beekeeper --config /etc/beekeeper/host.yaml agents hosts bundle > /srv/beekeeper/bundle.json
beekeeper --config /etc/beekeeper/host.yaml agents hosts prepare --bundle /srv/beekeeper/bundle.json
```

`prepare` prints the policy digest to pin in the coordinator catalog. Add the
following to the coordinator configuration, substituting that digest:

```yaml
agents:
  hosts:
    host-a:
      ssh: host-a.example
      executable: /usr/local/bin/beekeeper
      config: /etc/beekeeper/host.yaml
      identity: host-a
      workspaceRoot: /srv/beekeeper/workspaces
      repositories:
        example/repository: /srv/checkouts/repository
      harnesses: [claude]
      policyDigest: <prepared-policy-digest>
      maxBusy: 2
      connectTimeout: 10s
      operationTimeout: 30s
```

Coordinator and host releases may differ while they use the same execution
protocol. A coordinator self-update does not require reinstalling or re-preparing
the hosts. `hosts check` reports each host's release for diagnostics, and still
requires the expected identity, protocol, pinned policy, harness and fresh resource
observation. A different protocol is refused; upgrade the host to a release with
the coordinator's protocol before starting new work.

Changing the host binary, rules, settings, or protected host configuration invalidates
the policy. Prepare and pin a matching bundle again before starting new work.
Do not weaken a required guard to make a check pass.

## Codex on an execution host

A host may run Codex workers next to or instead of Claude. Install a pinned
Codex CLI in the supported family (`>= 0.165.0, < 1.0.0`) and add it to the
host's `harnesses`:

```yaml
agents:
  execution:
    harnesses:
      codex:
        executable: /usr/local/bin/codex
        version: 0.165.0
        configDir: /srv/beekeeper/codex
```

`configDir` is the host's own `CODEX_HOME` holding its login, and it is
required. Logging in is a one-time step for the person, on the host, with
the device flow; credentials are never copied from the coordinator:

```sh
CODEX_HOME=/srv/beekeeper/codex codex login --device-auth
```

Workers never run in that home. `prepare` installs a managed `CODEX_HOME`
inside the policy directory with the embedded worker rules as `AGENTS.md`, a
`config.toml` with hooks on and the browser, computer, code-mode, image and
plugin tools off, and a `hooks.json` whose PreToolUse hook is the execution
guard in Codex's format. It links only `auth.json` from `configDir`. The policy
binds those files; a changed one fails `hosts check` until a matching bundle is
prepared again. Use a ChatGPT subscription login: an API-key login is refused.

`hosts check` probes each configured harness's version, login and guard
without a model call. It advertises only the harnesses that pass and names
the others, with their reason, under `unavailable`. The coordinator lists the
harnesses it may start there, for example `harnesses: [claude, codex]`, and
starts a harness only when both list it and the host's check passes; `--harness
auto` chooses among those.

A remote Codex worker runs one `codex exec --json` turn per message. The first
turn carries the brief and creates the thread; the runner records it, binds the
guard to it, and runs each stored wake as `codex exec resume` on that thread. A
wake is accepted once its turn has started on the recorded thread; a turn on
another thread, or one that ends before it starts, leaves the receipt uncertain
for explicit resolution. A finished report ends the runner after its turn.
Lease, publication, review, merge, stop and recovery are the same as for
Claude. Codex usage stays on the coordinator: the remote worker counts against
the configured `codex` provider's ceiling, and the coordinator's own usage
reading covers the shared subscription; a host adds machine room only.

## Start, inspect, and recover

Run the coordinator's commands against its configuration:

```sh
beekeeper agents hosts check host-a
beekeeper capacity --host host-a --builds
beekeeper agents start --host host-a --repo example/repository --ref main --harness claude --model opus --mode headless worker /srv/coordinator/brief.md
beekeeper agents inspect worker --json
beekeeper agents logs worker --json --limit 65536
```

Use `--dry-run` to validate placement and print the proposed start without a
reservation, workspace, or process. Remote starts require `--repo` and `--ref`;
`--dir`, desktop, and background modes are refused. Capacity uses the
coordinator's existing provider and bucket pool across hosts and the selected
host's physical guards. A host does not create another subscription budget.

`supervisor loop` places its worker starts on execution hosts. With
`supervisor.hosts` naming execution hosts (in order of preference), a start
whose repository a host maps, and for which `capacity --host <selector>` has
room, runs there pinned to the repository's default branch — while the
coordinator is out of room, or always with `supervisor.hostsFirst`. A host's
check weighs only the choices it can run (Claude Code). `hostsFirst` prefers
hosts only for what a host can run: without room on any host a Claude start
waits, while a repository no host maps, or a choice of a harness no host has
an adapter for (agy, omp, codex), starts on the coordinator within its local
room; for the latter the local start excludes the choices a host runs. A desk
without `capacity.choices` never places remotely. Reviewers stay on the
coordinator even with `hostsFirst`: execution hosts have no relayed GitHub
read-and-review path. A review waits when coordinator review capacity is full.

`capacity.local.ceiling` caps the busy agents the coordinator itself runs,
apart from `capacity.ceiling`: a desk whose ceiling counts the hosts' slots
too sets it to the coordinator's own share. Remote agents count against
their host's `maxBusy`, never against it; local self-hosted sessions count.
Every local start, the loop's and one by hand, is refused at it. A retired remote worker is
stopped and acknowledged before it leaves the roster.

Preparation creates no model process. The coordinator first records the stable
execution reference and handle, then signs a short-lived registration receipt.
The host verifies the receipt and checks live coordinator authority before the
owned service and harness turn start. Success requires first-task acceptance
under the expected conversation identifier.

After an interrupted start, inspect and recover the existing execution:

```sh
beekeeper agents recover worker
beekeeper agents hosts sync host-a
```

Recovery retains the execution reference and pinned workspace. It never starts
a replacement for an uncertain running unit. A failed or partial workspace is
preserved for inspection. Do not issue another start under a new identity to
escape an unknown outcome.

Notes, parks, reports, and handover events from a managed worker reach the
coordinator through a durable ordered outbox. Failed delivery is explicit and
never falls back to host-local scheduling state. `hosts sync` and the watch
reconcile pending controls and stop intent. For a caller retry after lost shell
output, use the same `--request-id` and identical request; acknowledged controls
return their original receipts, including note numbers.

```sh
beekeeper agents stop worker --reason "operator stop"
beekeeper agents inspect worker --json
```

Stop intent is durable before SSH delivery. Only the recorded owned service may
be stopped. An unreachable host retains its reservation and task; reachability
is reported separately from the last process evidence. An acknowledgement of
termination is retained even if the subsequent inspection disconnects. Stopping
a process does not mark its task complete or remove its workspace.

Capped runs verify their worker identity, installed policy and live coordinator
authority before taking a slot. Their scopes stop with the verified owned
service and require that service to be active; they do not restart an inactive
owner. Local runs acquire no such dependency. The remote guard also inspects
commands nested inside `beekeeper run`, so wrapping an unavailable lifecycle or
merge operation does not authorize it.

The component does not adopt or archive personal sessions. Interactive remote
tools and remote transcript reads through coordinator-local paths are unavailable. Use owned execution
logs for output. Hooks inspect the supported tool surface and deny unavailable
authority; arbitrary code and account-owner changes require OS isolation for a
stronger boundary. New executions stop on coordinator lease expiry as described below.

## Deliver and reconcile messages

New SSH starts use one owned runner and one streaming Claude conversation. A
wake, note answer, timer, or park resume enters the coordinating outbox before
transport delivery. The host stores the envelope before returning `stored`.
Only a replay event with the exact message UUID and conversation ID establishes
`accepted`; worker mutations retain their separate `applied` control receipts.
The original sender, execution host, worker and generation remain attached to
the envelope. Local wake behavior is unchanged.

```sh
beekeeper --request-id follow-up-one agents wake worker "Continue the task"
beekeeper agents messages worker
beekeeper agents hosts sync host-a
beekeeper agents inspect worker --json
```

`agents messages` shows coordinating delivery state without message text.
`agents inspect` shows host receipts, ordered acceptance cursor and process
reachability. States are `pending` (coordinator only), `stored`, `uncertain`
(a harness write was attempted), `accepted`, `resolved` (an operator skipped a
blocked receipt without claiming acceptance), and `failed` (an unsent message found
its runner already completed or ended). A failed receipt blocks later delivery
and is not retried automatically. An unavailable or ended process
is also visible through inspection; a transport failure never proves it ended.
Wake exits nonzero until acceptance is confirmed. `hosts sync` and the watch
reconcile the same durable identities, retry only unacknowledged envelopes, and
never create a replacement process. A park remains until its resume is accepted.

An explicit request ID binds the sender and text; conflicting reuse is refused.
Without one, each CLI wake is a distinct send, including identical text. Use an
explicit request ID for CLI retries, or let `hosts sync` retry the persisted
outbox. Timer fires use their timer ID and original due time, note answers use
their note/decision identity, and resumes use their original park identity.
Repeating timer fires and separate CLI sends therefore remain distinct. Receipts
and the acknowledged cursor remain for the execution's lifetime; sequence gaps
stay queued and cannot overtake an uncertain message.

The runner persists `uncertain` before writing to the harness and commits an
ID-bearing acceptance evidence file before the journal receipt. Synchronization
recovers a receipt lost after that evidence. If the harness might have consumed
a message but no durable evidence survived, it remains uncertain for operator
reconciliation: inspect the owned logs, then explicitly resolve the oldest
blocked receipt if its outcome has been reconciled:

```sh
beekeeper agents messages resolve worker <message-uuid> --reason "Reconciled outcome; skip without replay"
beekeeper agents hosts sync host-a
```

This records the reason in both journals, retains the receipt, and advances the
cursor so later messages can flow. An uncertain write may already have been
consumed, or may still finish; resolution neither cancels it nor resends it.
Resolution does not count as acceptance or clear a park. If a deliberate new
resume is needed, use a new explicit request ID. Resolve retries reuse the same
message ID and reason. A different reason is refused. Completed and terminal
executions skip message delivery; control reconciliation and inspection continue
despite any message failure. Resolution never restarts an exited process.
Accepted and resolved coordinator receipts discard message text while retaining
the digest required to reject conflicting explicit retries.

No automatic replay or new process can resolve ambiguous consumption safely.
This is not a claim of crash-atomic model
execution. Journal admission refuses before exceeding its bounded storage.

Executions created before streaming-message support retain their original
single-turn behavior and explicitly refuse remote wakes. Recovery does not
upgrade or restart them.

## Coordinator leases and disconnected stops

New starts require a signed 90-second coordinator lease, renewed every 20 seconds.
The grant names the coordinator, execution host, execution ID and process generation.
The coordinator stores its sequence before replying; a lost reply retries the same
nonce and receives the original deadline. Stop intent and global stop revoke renewal
in the same coordinating state transaction. An unreachable stop keeps its capacity
reservation and unfinished task until termination is observed.

The runner measures its deadline from before sending the renewal using elapsed
monotonic time. A retry retains that original start time, so a delayed or repeated
reply cannot extend authority. Host/coordinator wall-clock offset does not extend
runtime. The signed issue/expiry times shown by inspection use coordinator UTC.
The runner checks expiry once per second, records stop intent, and asks the user
service manager to stop only its owned service and dependent build scopes. With a
responsive service manager, the disconnected bound is 90 seconds plus the one-second
check interval and the service's 60-second termination timeout. Service-manager
failure remains explicitly unconfirmed; it is not a termination acknowledgement.
A stop of its own unit is an orderly owned stop, not a crash: the runner records
the journal as `exited` with the recorded stop reason and exits 0, so a lease-expiry
stop leaves no failed unit to mistake for a harness crash.
Host suspension pauses monotonic elapsed time on platforms where the OS does so;
this bound assumes a running host and functioning user service manager.

A runner restart cannot reuse runtime authority: the recorded phase refuses another
runner, and systemd ends the original service's child processes when it exits.
Coordinator restarts retain the signed sequence and stop intent. Reconnection uses
`agents hosts sync <host>` and `agents recover <worker>` to inspect the same execution;
it does not replace an uncertain process. Inspection exposes the last grant, expiry,
reachability and stop acknowledgement. Older executions remain unchanged and require
an acknowledged explicit stop; recovery does not silently upgrade their contract.

## Live merge authorizations

Only the supervisor loop, or a person on the coordinator (`--as <person>`), issues or
revokes authorizations; a session never does. A green exact
head and an authenticated independent desk review result are prerequisites. The
component records the exact reviewer session when the supervisor starts the round.
That session must complete with the same `Review: approved` or
`Review: changes requested` verdict on the first report line, followed by the
exact public review URL. The reported verdict must match the public review.
An identical completion retry retains the original captured result; a later
shared-account edit cannot rebind that retry to another verdict. The
coordinator persists the GitHub review ID, author, head and body digest before
report delivery can prune the report. A shared GitHub login or a worker-authored
approval comment alone grants nothing; `--as` cannot submit the reviewer's result.
Issuance and live use require that same persisted result, and grants pin the
round ID, review ID and authenticated body digest. Edited reviews and new rounds invalidate old grants. Older
rounds without component-recorded reviewer provenance need a fresh review. The supervisor loop issues a two-minute grant when it gives a
remote worker merge go; a person can also issue or revoke it explicitly:

```sh
beekeeper --as <person> agents authorizations grant worker example/repository 42 --head <reviewed-head>
beekeeper --as <person> agents authorizations inspect <authorization-id>
beekeeper --as <person> agents authorizations revoke <authorization-id>
```

The remote worker uses the component command supplied with its go:

```sh
beekeeper agents merge example/repository 42 --head <reviewed-head> --authorization <authorization-id>
```

The command verifies owned process and repository identity on the execution host,
then authenticates its exact execution and generation to the live coordinating
service. It uses the coordinator's independently provisioned GitHub login: no host
credential is copied or forwarded. The actual merge runs synchronously at the
coordinator. Host hooks continue to deny raw `gh`/`devctl` merges and API mutations.
This narrow component path is the only supported remote merge surface; unsupported
harnesses and older executions remain denied.

The service checks the exact tuple, unexpired and unrevoked grant, current head,
CI, authenticated review, execution lease and stop state immediately before the
attempt. Holds use the same repository, lane and installation scope, PR/session
exceptions and explicit repair-window readiness waiver as the existing merge gate. It retains the coordinating state lock through a bounded synchronous
attempt (at most 20 seconds, also bounded by grant expiry), serializing stop,
hold changes and revocation against use. Those operations may report a busy state
lock during that interval and must be retried; they are never reported committed
early. The service consumes the grant durably before calling GitHub once and
never retries a merge mutation. A moved head needs a fresh review and go. A cached
grant provides no authority without this live service. Both host and coordinator
retain the authorization ID in their audit evidence.

A lost reply is an uncertain outcome, not permission to retry a merge. A person on the
coordinator queries the result and retains the consumed authorization:

```sh
beekeeper --as <person> agents authorizations reconcile <authorization-id>
```

Reconciliation never issues another merge. If GitHub cannot confirm the exact merged
head, the outcome remains unconfirmed and needs operator investigation. A failed or
consumed grant does not become reusable. Hooks cannot constrain arbitrary unhooked
programs or an account owner changing binaries, configuration or state. Dedicated
accounts or OS isolation are required for a stronger same-account boundary.

## Publish a branch and pull request

A managed Claude worker needs no GitHub write credential on the execution host.
Commit changes in the owned workspace, then run:

```sh
beekeeper agents publish --title "fix: resolve the defect" --body-file /srv/beekeeper/workspaces/<execution>/workspace/.beekeeper-scratch/pr.md
```

The coordinator receives a Git bundle over the authenticated callback, verifies
its HEAD and ancestry from the pinned execution commit, and pushes with its own
GitHub login. The remote branch is always
`beekeeper-execution/<execution-id>/<generation>` in the execution's repository;
a worker cannot select a repository, remote, destination ref, default branch,
force option or arbitrary GitHub API operation. The coordinator resolves the
repository's default branch and creates the PR against it. Both PR text and
new commit text pass the ordinary outbound rules; complete commit messages are
scanned separately from patch content, so message bullets are never mistaken
for patch deletions. PR text cannot ping accounts.

The first publication requires a title and body file. Once a PR exists, review
fixes need only `beekeeper agents publish`: the same branch advances by a normal
fast-forward push and the existing PR is returned without changing its text or
discussion. Closed PRs cannot be reused. For a lost push, create or callback reply,
repeat the publication: Git rejects history rewrites, and the coordinator looks
up the exact owned branch before creating a PR. A create attempt is never retried
blindly. PR editing and commenting continue to use coordinating workflows;
this command grants no general GitHub API authority.

To bring the latest default branch into the workspace before publishing again:

```sh
beekeeper agents publish --sync-base
```

This transfers upstream commits through the same coordinator, fetches them into
the owned workspace and merges them with hooks disabled. The merge resolves
the worker's usual author and committer identities, including global Git identity,
and supplies only those identity fields to the isolated Git operation. Other
global configuration stays disabled. Resolve conflicts and
commit locally, then publish. No source checkout is modified and no host GitHub
credential or SSH agent is forwarded. This command creates no review or merge
authorization; park on the returned PR and follow the independent review gate.

Each transfer uses authenticated 256 KiB chunks, within the existing wire bound,
and a SHA-256 digest over the complete bundle. A bundle is bounded to 64 MiB;
a larger transfer is explicitly refused rather than truncated or sent with a
credential workaround. Chunk retries verify existing bytes; partial or corrupt
bundles cannot be published. Transfer files remain under the coordinator's
`execution-publications/transfers` state directory for recovery and may be moved
aside once the execution is retired. Failed staging directories remain for
inspection; successful staging directories are removed.

Every chunk and publication checks the exact execution generation, repository
allowlist, current lease, completion, execution/global stop and GitHub hold.
Publishing holds the coordinating state lock through a synchronous attempt of
at most 20 seconds; it returns an explicit uncertainty on disconnect. Stops and
holds serialize with that attempt. `agents hosts check` refuses hosts that do
not advertise publication support; upgrade and prepare an older host before
starting workers there. Existing raw merge and API guards remain in force.
