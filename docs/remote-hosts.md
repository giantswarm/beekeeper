# Remote hosts and the central instance

Workers can run on SSH execution hosts, and several desks can share one central instance.

## SSH execution hosts

The first [SSH execution-host slice](ssh-execution-hosts-operations.md) starts managed headless
Claude sessions in isolated host-local clones, with coordinator registration, shared capacity,
acknowledged controls and owned stops. Selected coordinator [MCP tools and
connectors](remote-mcp.md) are available through the worker's authenticated SSH callback. Remote
wakes and resumes use durable ordered message receipts; inspect them with `agents messages` and
reconcile with `agents hosts sync`.

Hosts follow the coordinator's release: `agents hosts update` installs it there, verified, and pins
the prepared policy digest in beekeeper's state, which the supervisor loop does after each
coordinator update settles probation. Compatible older hosts keep taking starts during probation or
release lookup failures; failed installations or policy checks still hold the host. After
probation an older host drains: it takes no new start until it updated, so its executions reach
the turn boundary the update waits for, and one behind for over an hour is reported once. With
`supervisor.hosts`, workers prefer the first host with room by default and fall back to the
coordinator within its ordinary capacity guards. Starts by hand without `--host` (`agents start`,
`emergency start`) are placed the same way and say where they went; `--host local` keeps one on the
coordinator.

Explicit `supervisor.hostsFirst: false` retains local-first placement. `capacity` and `supervisor
status` show dated per-host placement reasons. Reviewers follow the same host preferences, including
Codex reviewers without a worker GitHub App. On an execution host, `beekeeper agents review` reads
the assigned PR and its closing issues, `--diff` reads the diff, and `--body-file <file>` posts the
exact-head comment review through the coordinator for merge authorization.

These execution forms take no PR reference: the assigned review supplies the target. On the
coordinator, `beekeeper agents review <owner/repo>#<n>` starts an independent review; its flags
cannot be mixed with `--diff` or `--body-file`. The [design](ssh-execution-hosts.md) records the
contract and the remaining coordinator lease and merge authorization work.

## The central instance

A local lease's purpose may name its round, for example `60m`, `1h30m`, or
`60-minute round`. The first positive duration is used; without one,
`leaseRound` defaults to `1h`. Once past that round, `watch` prints
`LEASE HELD PAST ROUND` once for the claim, naming the holder, claim time,
purpose and overrun. Queued claims include the same overrun, including when
`--wait` ends queued. This is a report only: the holder keeps the lease.

The leases of the installations a team shares live in one place, `beekeeper serve` behind muster, so
two people's machines see one holder; its tools, as a model reads them, are in
[docs/tools.md](tools.md). `central: {context: <muster context>}` turns it on: `lease
claim|release|status` of a name that is none of the machine's own resources (`resources`, the model
server, the browser) goes to the central instance as the person, through muster's `call_tool`
(`x_<central.server>_<tool>`).

The person signs in once with `muster auth login --context <context>`; each call takes the context's
endpoint and the person's token from `muster` and keeps neither. If muster refuses a call with
`auth_required`, beekeeper asks `core_auth_login` on that same connection to reuse the person's
existing grant, then retries the refused call once only after muster confirms the connection. It
never retries an ambiguous failure or opens a browser. If a grant cannot be reused, the error names
`muster auth login --context <context> --server <server>`; for an SSO server, renewing the muster
sign-in with `--force` may be needed.

Authentication refusals say the requested operation did not run, rather than suggesting an answer or
claim may have been recorded. `lease list` shows the machine's leases, then the central ones. A
claim another person's agent holds is refused (exit 3) with its holder, `"<person>/<agent>"`; a name
the central instance does not know is a usage error (exit 2). As a local claim, a central claim
needs no grant: a free resource is the claimant's.

Central access is **host-only**. Sandboxed sessions cannot use central notes, roster,
holds, leases or subscriptions: the sandbox cannot access the host's muster session
and socket, and no central broker route or token handoff is provided. Adding a
central endpoint to `sandbox.domains` does not enable access. Run commands that
need central state on the host outside the agent sandbox. Sandboxed central calls
fail with an explicit host-only message (exit 69 for commands requiring central);
commands that also show local state retain their existing local fallback behavior.

There is no local copy to fall back on: a central verb that does not reach the central instance
(no sign-in, muster or `beekeeper serve` not answering) is refused with the reason and exit 69,
and the machine's own resources are unaffected. `beekeeper watch` publishes the machine's agents
to the central roster on their transitions only (registered, a task taken or ended, done, gone;
`agents_register` with `task` and `done`, `agents_leave`), and says `CENTRAL UNREACHABLE
<context>: <reason>` once when the central instance stops answering, and its `ENDED` line once it
answers again; what was not published meanwhile is published then. A watch with nothing to
publish asks every five minutes.

Every watch prints the central instance's feed. It subscribes to `beekeeper://feed` through muster
as the person and, once the subscription stands and on each update, reads it (`read_feed` through
muster's `call_tool`): each event the person may read prints once, as `CENTRAL <line> [<person> on <host>]`
with the actor's person and host. A session's watch keeps the last event's id in its mark, so a
restarted watch prints what happened while it was down and nothing twice, however many events that
was; a watch without one starts at the newest event.

When the central instance no longer holds the last event it printed (down longer than the API server
keeps Events), the first read of a subscription says `CENTRAL FEED GAP <context>: events after <time>
may be missing` before the events since. A subscription that ends on an unreachable central instance
is the `CENTRAL UNREACHABLE` line, one the instance refused `CENTRAL FEED REFUSED`; the watch
subscribes again after a minute.

The standby watch delivers the person's mailbox to the machine's agents. It subscribes to
`beekeeper://mailbox/<person>` through muster (`subscriptions/listen`, MCP 2026-07-28) and, once the
subscription stands and on each update, calls `receive_messages`: each message addressed to one of
the machine's agents (`local:<host>/<name>`) arrives in that agent's running session at its next
tool round, else as a wake's turn, as `Message from <person> via slack (<id>): <text>`, and is acked
once delivered (`MAILBOX to <name>: …`).

One to a role's run (`Guide run <n>`) reaches the role's holder now, as a note's answer does on the
machine (`MAILBOX to <holder>, the <role> now, for <name>: …`); while no session holds the role, or
its holder does not take it, it waits for the role's next start, acked (`MAILBOX <id> to <name>
waits for the <role>'s next start`, `mailbox.kept`). A message its agent does not take stays, with
the later ones to that agent in their order (`MAILBOX <id> waits for <name>`), and is delivered
again every `watch.interval`; one to another machine's agent is left to that machine; an expiry
notice is said and acked.

A subscription that ends is one `MAILBOX UNSUBSCRIBED` line; the watch subscribes again after a
minute and drains every `watch.interval` meanwhile. An agent writes to its person in Slack with
`beekeeper converse "<text>"` (the central instance's `converse`): the guide's questions open its
thread, and the person's replies there come back the same way.

The central instance's notes stand beside the machine's. `note list`, `guide queue` and `guide next`
read its open notes (`note_list`, through muster as the person) after the machine's, each named
`<context>#<n>` (`--json`: its `central`). `note answer <context>#<n>` answers one as the person
(`note_answer`: only its addressee may), which closes its Slack message and tells its filer; `note
done <context>#<n> --reason "<why>"` withdraws one (`note_done`: its filer, or the filer's team's
supervisor role).

The tool requires `reason`; it is retained in the closure, shown by `note show` and included in the
closed Slack message. A bare `<n>` is the machine's note when it has one, else the central
instance's; one open on both is refused, naming both. `--post` and `--overtaken` are the machine's
notes' only. With the central instance unreachable, `note list` prints the machine's notes and exits
69; `guide queue` and `guide next` warn on stderr and serve the machine's.

A lane whose installation is central queues centrally too, so two people's merges into it roll one
after the other. The gate queues the merge in the central lane (`lane_queue`) as well as the
machine's, asks for its turn there every 15 seconds while it heads the local lane, and runs devctl
only when the merge is next in both. Behind a live local gate those checks back off to one minute,
always within half of `merge.queueTTL`. It makes it the central lane's running merge
(`lane_settle`), reports its outcome (merged: the lane settles; otherwise it leaves with
`lane_leave`), and the watch takes it out once its release rolled on the installation.

A waiting place whose gate stopped asking for `merge.queueTTL` holds up nobody. A hold on a central
lane (`hold set --lane`) or on a repository in one is central: `hold set|lift|check|list` go to the
central instance, and its gate refuses the merge with the hold (exit 77) on every machine;
`--lift-when`, a probe on one machine, is refused for a central target. `lanes central` lists the
central lanes, and `lanes leave owner/repo#n` takes a merge out of its central lane by hand (its
person's, or the team's supervisor role's). A gate that cannot reach the central instance refuses
the merge with exit 69 and queues nothing.
