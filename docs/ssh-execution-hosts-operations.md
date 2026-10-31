# Operating SSH execution hosts

Managed, headless Claude Code sessions run on an explicitly selected SSH host.
Each execution gets an isolated clone, an owned user service,
a conversation identifier, and one coordinator roster registration. Omitting
`--host`, or selecting `--host local`, keeps the local launch path.

Read the [design](ssh-execution-hosts.md) for the ownership and failure contract.
Remote wake and resume use durable message receipts as described below. The expiring coordinator lease
is tracked in https://github.com/giantswarm/beekeeper/issues/1715, and live remote merge
authorization to https://github.com/giantswarm/beekeeper/issues/1716. Every remote
merge is refused in this slice, including when a local approval exists.

## Provision the components independently

Install the same beekeeper version on the coordinator and execution host. The
host needs Linux with a working user service manager, Git, and a pinned Claude Code version
in the supported family (`>= 2.1.268, < 3.0.0`). Provision its subscription login
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

Changing the binary, rules, settings, or protected host configuration invalidates
the policy. Prepare and pin a matching bundle again before starting new work.
Do not weaken a required guard to make a check pass.

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
stronger boundary. A disconnected stop has no automatic lease expiry until its
linked follow-up ships.

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
