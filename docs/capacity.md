# Capacity, models and subscriptions

How many agents run, which model and subscription each start takes, and what they cost.

## Task models

`agents.models` selects a model by the start's name regexp and optional task
kind; `--model` overrides it. `agents.requireModel: true` refuses an unresolved
model with exit 3. `guide.model` overrides the previous holder for relay and
standby successors. The supervisor loop uses the same model policy for its
workers and reviewers.

`capacity.choices` can describe model class, relative cost, allowed task kinds,
family and weekly review reserve; a choice on a self-hosted provider takes code
changes (kind `worker`) only when its `kinds` list it. Every start, the loop's
included, picks the eligible subscription first and a self-hosted choice only
when no subscription is eligible. `capacity.taskPolicies` constrains task kinds;
`--kind` selects the policy for both capacity and starts. Review starts can use
`--author-model` to require another model family. Capacity JSON explains the
choice and rejections; successful starts record the model and reason.
See [the task model policy](development.md#task-model-policy) and
[example configuration](examples/config.yaml).

## Task kinds

`agents.kinds` names kinds of agents by name regexp, each with `maxBusy`,
`minInterval` and `dailySpend`; a name several kinds match is held by each.
`beekeeper capacity` prints `no start <kind>: <reason>` for a kind at a cap,
and `agents start` refuses a start of its agents with that reason, exit 3,
`--force` included. `beekeeper sessions --by kind` prints a day's starts,
hours and spend per kind, and the watch prints `SPEND <kind>` once a day when
a kind passes its `dailySpend`. See [task kinds](development.md#task-kinds).

## The model server

The host's model servers (ollama, and Lemonade Server where `lemonade.url` is set) keep a
loaded model in the iGPU's memory: system RAM the GPU driver pins, which no cgroup counts
or caps. One lab request that names a large model
can take the desktop into systemd-oomd. So the model server is a resource like a lab:
a session that loads a model on it holds the `model-server` lease, and the claim says how
much it may load.

```sh
beekeeper lease claim model-server -p "models-test for model-manager#219" --gib 10 && agentlab models-test
beekeeper lease release model-server
```

- The budget defaults to `ollama.budgetGiB` (12); a claim above `ollama.maxBudgetGiB` (24) is
  refused.
- `beekeeper watch` compares ollama's loaded models (`/api/ps`) with the lease on every
  sample. Ollama's journal names the client of each load once its request ends; while it
  still runs, the one client connected to the model server's port is its client (none when
  several are connected). A kind node's address maps to
  its lab (the cluster's name, or `<name>-1` for the first of a numbered series) and to
  the lab's holder. A model loaded while nobody holds `model-server`, loaded by a lab
  another session holds, or beyond the holder's budget (the largest first) is a
  `MODEL SERVER` line. The watch unloads it (`keep_alive: 0`; the weights stay on disk),
  unless `ollama.nameOnly` is set. A load from the host itself without a lease is only
  named: a person may be using it.
- Lemonade's loaded models (`/api/v1/health`, sized from `/api/v1/models`) count against
  the same lease and budget as ollama's, and the watch unloads them through
  `/api/v1/unload`. Lemonade logs no client address: a model's client is the one client
  connected to its port, none when several are. `snapshot` and the `IGPU GTT` line list
  every server's models under its name; one server not answering hides none of the
  other's.
- The PreToolUse hook refuses, outside the session holding `model-server`,
  `ollama run|pull|create|push|cp`, `lemonade run|launch|chat|load|pull|bench`, an HTTP
  request to a model server's port on a generate, chat, embed, pull or `/v1/` path (for
  Lemonade `/api/v1/` chat, completions, responses, embeddings, reranking, load, pull,
  audio and images), and the agentlab tests in `ollama.labTests`. `ollama ps`,
  `ollama stop`, `/api/ps`, `lemonade list|unload` and `/api/v1/health` pass.

## Session metrics

How each session has been doing, read from what beekeeper already reads, never from GitHub:

- busy time (entries less than 5 minutes apart), turns, tool calls, tool errors and the most
  repeated failing call, GitHub calls (`gh` and `devctl` commands, tools of a GitHub MCP server):
  the transcript
- tokens (input, cache writes by TTL, cache reads, output) and cost: the transcript's usage fields,
  once per API response, priced from `metrics.models`
- context: the last request's tokens and their share of the model's window: the transcript
- idle time: the transcript's modification time
- memory: the process tree's anonymous memory, and each capped run's scope (`memory.current`):
  `/proc` and the memcap scopes its `run.start` events name
- `gh` and `devctl` processes: the process table, attributed as `budget` does
- merges queued, merged, refused and failed: the gate's events in the event log
- leases and how long each has been held: the lease directories

The transcript's figures cover its last 512 KiB, the window `sessions` already reads for what a
session is on (`since` in `--json` is its first entry, `whole` says it is the whole transcript),
and the last hour of it. `sessions --json` has them per session, `sessions --json` and `snapshot
--json` the totals; `snapshot` names the three sessions that spent the most in the last hour.

Cost is priced per response from the model's entry in `metrics.models` (US dollars per million
tokens). The defaults are the Claude API list prices; a model without an entry, or a fast-mode
response without a `fast` multiplier, makes the cost `cost unknown`, never a guess. There is no
Prometheus export: nothing on the lab machine scrapes one.
