# Remote worker resource retirement

A worker ends, and its host should hold only the provisioned sources, the
installed toolchains and beekeeper's own state. The
[retirement lifecycle](ssh-execution-hosts-operations.md) preserves the work and
moves the execution's resources aside; this page says what leaves the host and
what never does.

## What a worker writes where

Each execution owns the directory named by its execution ID beneath the
workspace root. Temporary files, Go build and module caches, downloaded Go
toolchains, `XDG_CACHE_HOME`, `XDG_STATE_HOME` and `CARGO_HOME` all point inside
it, so they retire with it. An agy worker always runs on a home of its own beside
its journal, with no MCP server configured unless the request asks for one.

`HOME` points inside it too, so what a worker writes to a literal `~` path retires
with it. The account's home stays where the host provisioned it: harness logins
live in each harness's configured directory, Git and GitHub configuration are
reached by explicit path (`GIT_CONFIG_GLOBAL`, `XDG_CONFIG_HOME`), beekeeper finds
its own state through `BEEKEEPER_EXECUTION_HOME`, and ssh reads the account
database, not `HOME`.

Two workers keep the account's `HOME`: a claude worker on a host with no
`configDir`, whose login lives there, and every worker on a host with
`sharedHome: true`, the way back if a tool on that host needs the account's home.
Both change the audit's reach, not its rules: a worker can also still write to the
account's home by absolute path. `hosts check` names what is in the account's
`~/.local/state`, where such downloads land, but cannot attribute it to an
execution, and a write elsewhere there goes unseen.

## What removes a retired resource

Only a retirement does, whether a coordinator asked for it or an operator ran
`agents hosts reconcile`. `Stop` ends the owned process and removes the
disposable MCP home; inspection observes. Neither touches a workspace, a source
bundle or anything moved aside, retention passed or not.

Retirement proves an execution disposable when it holds nothing a person could
want:

- no worker ever ran, or
- the tree is on its starting commit with no recovery to record, and it holds no
  ignored file beyond the designated scratch directory.

Once `retainUntil` passes, the next retirement or reconciliation on that host
removes the moved-aside resources of such executions, a few per call. A host that
does neither keeps them until it does. The journal, log and control receipts stay
as evidence.

Everything else stays moved aside without expiry: a retirement with a recovery,
one with ignored files, one whose tree could not be read, and every retirement
recorded before this proof existed. Its owner publishes or recovers the work
before removing it by hand.

A removal that fails its containment checks, or finds a symlink, leaves the
resource in place and records why in the retirement's `lastError`; the next
retirement tries again.

## Seeing what is left

`beekeeper agents hosts check <host>` reports what remains in `resourceAudit`,
and [its retention table](ssh-execution-hosts-operations.md#resource-audit-and-bounded-retention)
gives each class's owner and rule. Execution logs keep that table's policy,
including its explicit evidence release.
