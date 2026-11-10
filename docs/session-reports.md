# Session reports and state snapshots

The session CLI queues local `agents park`, `agents idle`, `agents problem`,
`note add`, and `sessions serve` reports for the supervisor. A successful
response means that the message was synced to the local inbox. It does not
mean that the supervisor has applied it yet. The response includes a message
ID; JSON output includes `id`, `reference`, and `status: queued`.

The supervisor applies messages in inbox order. Each state transition commits
its receipt and audit events together. A restart after publication retains the
message; a restart after the commit uses its receipt instead of applying the
transition again. The loop revalidates the session assignment and the command
against current state. A rejected message records `session.report-rejected`
and wakes its sender with the reason. Failed wakes remain pending for retry.

`note add` returns `report:<message-id>` before a note number exists. Use that
reference directly with `agents park --on report:<message-id>`. The loop
resolves it to the created note, including when the note folds into an
existing one. The park is rejected if the note cannot be created or is no
longer open.

`status`, `capacity`, `delivery`, `sim`, and watch output read the last atomic
state snapshot without acquiring the state lock or recovering an event
outbox. A snapshot older than one configured supervisor pass is identified on
stderr with its age. JSON output and the fixed status-bar fields retain their
shape. A snapshot can lag queued reports until the supervisor consumes them.

CI holds the exclusive state lock for two minutes while checking report and
read-command latency. It also checks ordered application, crash replay,
assignment replacement, and explicit subprocess, network, and sleep calls
inside state update callbacks. The static callback check detects explicit
calls; helpers still need review for transitive slow work.
