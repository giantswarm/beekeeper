# The tools

Generated from the registered tools of `beekeeper serve` (`make tools-doc`); through muster every tool is `x_<central.server>_<name>`. Every tool takes `agent` and `host`, which tell one person's agents apart.

| Tool | Kind |
|---|---|
| `ack_messages` | write, destructive |
| `agents_leave` | write, destructive |
| `agents_register` | write |
| `converse` | write |
| `hold_lift` | write, destructive |
| `hold_list` | read-only |
| `hold_set` | write, destructive |
| `lane_leave` | write, destructive |
| `lane_queue` | write |
| `lane_settle` | write |
| `lane_turn` | read-only |
| `lanes` | read-only |
| `lease_claim` | write |
| `lease_list` | read-only |
| `lease_release` | write, destructive |
| `list_agents` | read-only |
| `note_add` | write |
| `note_answer` | write, destructive |
| `note_done` | write, destructive |
| `note_list` | read-only |
| `read_feed` | read-only |
| `receive_messages` | read-only |
| `send_message` | write |
| `snapshot` | read-only |

## `ack_messages`

```text
Ack messages of your mailbox by their ids, so they are not delivered again.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "for": {
      "description": "the mailbox: your email (default)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "ids": {
      "description": "the ids receive_messages returned",
      "items": {
        "type": "number"
      },
      "type": "array"
    }
  },
  "required": [
    "ids"
  ],
  "type": "object"
}
```

## `agents_leave`

```text
Take the calling agent off the roster: its machine no longer runs it.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [
    "agent",
    "host"
  ],
  "type": "object"
}
```

## `agents_register`

```text
Register the calling agent on its team's roster: idle, or busy with its task.

A machine publishing its agents passes each one's task and whether it is done; an agent registering itself passes neither and keeps its task.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name",
      "type": "string"
    },
    "done": {
      "description": "the agent reported its work finished: ended on the roster",
      "type": "boolean"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "task": {
      "description": "the agent's task (a machine's agent): empty is idle",
      "type": "string"
    }
  },
  "required": [
    "agent",
    "host"
  ],
  "type": "object"
}
```

## `converse`

```text
Write to your person in Slack as the calling agent, in its conversation thread.

The first message opens the thread; the person's replies arrive in your mailbox from source slack.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "text": {
      "description": "the message, Slack markdown of at most 12000 characters",
      "type": "string"
    }
  },
  "required": [
    "text",
    "agent",
    "host"
  ],
  "type": "object"
}
```

## `hold_lift`

```text
Lift a hold: your own, or as its setter's team's supervisor role.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "target": {
      "description": "the held target",
      "type": "string"
    }
  },
  "required": [
    "target"
  ],
  "type": "object"
}
```

## `hold_list`

```text
List the holds on merges into a repository or lane, or on every merge, until lifted.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `hold_set`

```text
Hold merges in the central lanes: owner/repo, lane:<name>, or merges or github for all of them.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "except": {
      "description": "the one owner/repo or owner/repo#n the hold lets through",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "reason": {
      "description": "why",
      "type": "string"
    },
    "target": {
      "description": "owner/repo, lane:<name>, merges or github",
      "type": "string"
    },
    "until": {
      "description": "when it ends by itself: a time (15:30) or a duration (2h)",
      "type": "string"
    }
  },
  "required": [
    "target",
    "reason"
  ],
  "type": "object"
}
```

## `lane_leave`

```text
Take a merge out of its lane: it merged and rolled, failed, or its gate gave up.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "pr": {
      "description": "the pull request's number",
      "type": "number"
    },
    "reason": {
      "description": "why it leaves: rolled, nothing merged, refused",
      "type": "string"
    },
    "repo": {
      "description": "the repository, owner/repo",
      "type": "string"
    }
  },
  "required": [
    "repo",
    "pr"
  ],
  "type": "object"
}
```

## `lane_queue`

```text
Queue a pull request's merge at the end of its repository's merge lane.

Queue it again while it waits: a waiting place not queued again for merge.queueTTL (default 15m) holds up nobody.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "pr": {
      "description": "the pull request's number",
      "type": "number"
    },
    "repo": {
      "description": "the repository, owner/repo",
      "type": "string"
    }
  },
  "required": [
    "repo",
    "pr"
  ],
  "type": "object"
}
```

## `lane_settle`

```text
Record a merge run outside the gate as its lane's head: running, or settling once merged.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "merged": {
      "description": "the pull request is merged: the lane settles until its release rolled",
      "type": "boolean"
    },
    "pr": {
      "description": "the pull request's number",
      "type": "number"
    },
    "repo": {
      "description": "the repository, owner/repo",
      "type": "string"
    }
  },
  "required": [
    "repo",
    "pr"
  ],
  "type": "object"
}
```

## `lane_turn`

```text
Tell whether a queued merge is its lane's next, and which merges are ahead of it.

It does not keep the place: lane_queue does.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "pr": {
      "description": "the pull request's number",
      "type": "number"
    },
    "repo": {
      "description": "the repository, owner/repo",
      "type": "string"
    }
  },
  "required": [
    "repo",
    "pr"
  ],
  "type": "object"
}
```

## `lanes`

```text
List the merge lanes: each lane's running merge, settling merge and queue.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `lease_claim`

```text
Claim a shared installation for the calling agent.

Refused while another holds it or while it upgrades.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "environment": {
      "description": "the Environment, an installation's name",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "purpose": {
      "description": "what the installation is for",
      "type": "string"
    }
  },
  "required": [
    "environment",
    "purpose"
  ],
  "type": "object"
}
```

## `lease_list`

```text
List the shared installations: who holds each, since when and what for, and which are free.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `lease_release`

```text
Release an installation: your own, or as the holder's team's supervisor role.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "environment": {
      "description": "the Environment",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [
    "environment"
  ],
  "type": "object"
}
```

## `list_agents`

```text
List the agent roster you may read: your own local agents, your team's holding a lease, every remote one.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "scope": {
      "description": "all (default) or team: only the caller's team",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `note_add`

```text
File a decision for a person or a team, put to them in Slack: its question, due time, default and thread.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "checked": {
      "description": "where a merged, green, released, rolled or closed claim was checked",
      "type": "string"
    },
    "default": {
      "description": "the action if nobody answers by the due time",
      "type": "string"
    },
    "due": {
      "description": "when it is due: a time (22:55) or a duration (3h)",
      "type": "string"
    },
    "fold_into": {
      "description": "an open note for the same person this one folds into, whatever it asks: that note's default and recommendation stand, and its number is returned",
      "type": "number"
    },
    "for": {
      "description": "who decides: a person's name or email, or team:<name>",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "kind": {
      "description": "decision, the default and the only kind this tool files",
      "type": "string"
    },
    "options": {
      "description": "\"<choice>: <consequence>\" each, at most 10, labels at most 75 characters",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "recommend": {
      "description": "the option recommended, 1-based",
      "type": "number"
    },
    "refs": {
      "description": "the issues and pull requests it asks about, owner/repo#n",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "status_quo": {
      "description": "what is true now",
      "type": "string"
    },
    "text": {
      "description": "the question (one line, at most 150 characters)",
      "type": "string"
    },
    "why": {
      "description": "why it needs the person",
      "type": "string"
    }
  },
  "required": [
    "text",
    "for"
  ],
  "type": "object"
}
```

## `note_answer`

```text
Answer a decision in the person's words and close it: as the person it is for, or a member of the team it is for.

A decision its default closed stays answerable for 24h: the answer replaces the default.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "choice": {
      "description": "the option chosen, 1-based",
      "type": "number"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "note": {
      "description": "the note's number",
      "type": "number"
    },
    "text": {
      "description": "the answer in the person's own words, at most 3000 characters",
      "type": "string"
    },
    "via": {
      "description": "how the answer came: cli (the default) or slack",
      "type": "string"
    }
  },
  "required": [
    "note"
  ],
  "type": "object"
}
```

## `note_done`

```text
Withdraw a note, closing it without an answer: its filer's, or the filer's team's supervisor role's.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "note": {
      "description": "the note's number",
      "type": "number"
    }
  },
  "required": [
    "note"
  ],
  "type": "object"
}
```

## `note_list`

```text
List every open note, whoever it is for or filed it: decisions and sign-ins.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `read_feed`

```text
Read the feed: the most recent events you may read, oldest first, as beekeeper://feed holds them.
```

```json
{
  "properties": {
    "after": {
      "description": "only the events whose id is above this one",
      "type": "string"
    },
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `receive_messages`

```text
Read the oldest unacked messages to any of your agents, in order; ack only those addressed to you.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "for": {
      "description": "the mailbox: your email (default)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "limit": {
      "description": "at most this many (default and most: 50)",
      "type": "number"
    }
  },
  "required": [],
  "type": "object"
}
```

## `send_message`

```text
Send an A2A message to an agent: one of your own local agents, or a kagent session.

A local: message is queued in your mailbox, a messageId sent again within 24h not queued again; a kagent: one goes through muster as you.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "deadline": {
      "description": "when an unacked local: message expires: an RFC 3339 time or a duration (2h); default 24h",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    },
    "message": {
      "description": "the A2A Message: messageId, role and parts",
      "properties": {},
      "type": "object"
    },
    "to": {
      "description": "the address: local:<machine>/<name> or kagent:<installation>/<namespace>/<session>",
      "type": "string"
    }
  },
  "required": [
    "to",
    "message"
  ],
  "type": "object"
}
```

## `snapshot`

```text
Show everything at once: leases, holds, lanes, every open note and the agents you may read.
```

```json
{
  "properties": {
    "agent": {
      "description": "the calling agent's name (default: your email)",
      "type": "string"
    },
    "host": {
      "description": "the machine or installation it runs on",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```
