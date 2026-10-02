# The feed and the resources of `beekeeper serve`

`beekeeper serve` offers its state as MCP resources. A client subscribes with `subscriptions/listen`
(MCP 2026-07-28, the only revision served), gets `notifications/resources/updated` with the URI of
each resource that changed, and reads it. Nothing is polled.

| Resource | Who may read and subscribe | Updated when |
|---|---|---|
| `beekeeper://environments/<name>` | every member | the Environment changes: holder, grants, upgrades |
| `beekeeper://lanes/<name>` | every member | the MergeLane changes: its merge, its queue |
| `beekeeper://roster` | every member | a RosterEntry is added, changed or removed |
| `beekeeper://feed` | every member | an event is recorded |
| `beekeeper://notes/<person>` | that person | a note for them, for their team (`team:<team>`) or filed by them changes |
| `beekeeper://mailbox/<person>` | that person | a message arrives, is acked or expires |

A read or a subscription of a person's resource by anyone else is refused, and a subscription
naming one such URI is refused as a whole. Every update carries the `subscriptions/listen`
request's id in `_meta` (`io.modelcontextprotocol/subscriptionId`).

## The feed

Schema `beekeeper.giantswarm.io/feed/v1`. The feed holds the 200 most recent events of the
shared state, oldest first: the changes (a claim, a release, a note, an expired message), not the
audit of calls that changed nothing.

```json
{
  "schema": "beekeeper.giantswarm.io/feed/v1",
  "events": [
    {
      "id": "1790949600123456789",
      "kind": "lease.claim",
      "subject": "Environment/staging",
      "actor": {"name": "ana-agent", "person": "ana@example.com", "team": "platform", "host": "laptop"},
      "time": "2026-10-02T12:00:00.123456Z",
      "line": "lease.claim (ana-agent): staging: e2e for #123"
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `id` | A decimal string of 19 digits that only grows, across restarts too: sorted as text or as a number, it orders the events. A reader prints the events whose id is above the last one it printed. |
| `kind` | The verb, `<area>.<what>`: `lease.claim`, `lease.release`, `hold.set`, `note.add`, `note.answer`, `agents.register`, `message.expired`, … |
| `subject` | The record the event concerns, `<Kind>/<name>`; a team's namespace (`Namespace/beekeeper-<team>`) for an event without one, such as `message.expired`. |
| `actor` | Who acted: the agent's name, its person (the verified email), team and host. |
| `time` | When, RFC 3339 in UTC. |
| `line` | The event as the local watch and log print it, without the time. |

Each event is a Kubernetes Event on its subject (`kubectl describe` shows it), carrying its id in
the annotation `beekeeper.giantswarm.io/id`; after a restart the ids continue above the highest
of the Events kept.

Within `v1` a change only adds optional fields. Anything else is `v2`, served beside `v1` until
its readers moved.

## The mailbox

`beekeeper://mailbox/<person>` reads `{"mailbox": "<email>", "pending": <n>, "cap": 50}`. After its
update the person's local beekeeper calls `receive_messages`, which returns the oldest unacked
messages in order, each with its `id`, and `ack_messages` with those ids.

- `send_message(to, message)` takes an A2A Message (`messageId`, `role`, `parts`). To
  `local:<machine>/<name>` it is queued in the mailbox of the person whose agent the roster names
  there, in an envelope `{"to", "from": {"person", "agent", "host"}, "message"}`. To
  `kagent:<installation>/<namespace>/<session>` it is sent through muster (`serve.muster`, the
  installation's tool in `serve.kagent`) to the session's `invoke_agent_instance`, with the
  caller's token.
- Delivery is at least once and ordered per sender and receiver. A mailbox holds at most 50
  unacked messages; the 51st is refused. A sender's `messageId` is accepted once within 24 h; a
  resend returns the first delivery's id, marked `duplicate`.
- A message is kept until it is acked or its deadline passes (`deadline`, else 24 h). An expired
  message leaves a `message.expired` event on the feed and an `expired` notice in the sender's
  mailbox, which does not count against the cap.
- The mailboxes live in the `beekeeper` database of the platform's Postgres, whose URL
  `BEEKEEPER_DATABASE_URL` names; `beekeeper serve` migrates the schema at start and refuses to
  start without it.
