# omp, agy and codex agents

Besides Claude Code, beekeeper runs workers on omp, agy (Antigravity CLI) and codex.

## omp agents

omp (oh-my-pi) is beekeeper's second local harness beside Claude Code, for agents on a locally
served open-weights model or any other provider omp speaks. Every omp process that runs a session
(no helper subcommand such as `omp models` or `omp ps`, no subagent of another omp) shows in
`sessions`, `snapshot` and `free` with harness `omp`, its state (`busy` while a turn runs, `idle`
while it waits for a message), its title, model, checkout and memory, the tool commands its shells
run, and its last hour from its session file (turns, tool calls and failed ones, GitHub calls,
tokens, and the cost omp itself counted). `tail <session>` follows it: the person's and beekeeper's
messages and its replies, without thinking and tool output.

omp keeps no record of which process runs which session file and holds none open; its session files
(`omp.sessionsDir`, `~/.omp/agent/sessions/<folder per cwd>/<time>_<id>.jsonl`) start with a title
line and a header naming the session id, its creation time and its working directory. beekeeper
gives each process the file of its working directory it created (the first created at or after the
process started), and a process without one the file there written last since it started (a session
it resumed). omp writes the file at the first reply, so a session not yet answered is listed
without a transcript.

`agents start <name> <brief> --harness omp [--model m]` starts `omp --mode rpc --no-ui --approval-mode
yolo --model <m>` on `--model`, else `omp.model`: an exact selector `omp models` lists
(`provider/id`, such as `ollama/qwen3.5:9b`). With neither, or a model omp does not list, the start
is refused before anything is recorded: omp's own default and a fuzzy pattern would pick a model
nobody named, which may not be served right now. `agents` and `sessions` show each session's model,
an omp session's from its command line until its session file records one.

It runs in a transient user unit `beekeeper-omp-<id>`, with its stdin on a FIFO inbox
(`<stateDir>/omp/<id>.in`) the unit opens for reading and writing, so the inbox never ends while the
agent runs. The agent carries its start id and name in its environment (`BEEKEEPER_OMP_AGENT`,
`BEEKEEPER_AGENT_NAME`) and is registered on the roster as `omp_<id>`. A message is one line of
omp's rpc protocol, a `steer` command: omp delivers it at the agent's next tool round while a turn
runs and starts a turn with it while the agent is idle.

The brief is the first message; `agents wake <name> <message>` writes the next. Writers take a lock
file beside the inbox, so two senders' lines never interleave. A wake of an agent whose process
ended is refused (exit 3): nothing reads its inbox, and nothing resumes it. Taking the agent off the
roster (`agents remove`, the doctor) stops its unit and removes its inbox and lock file: nothing
resumes an omp agent, so a process left running would only hold its model.

The agent's own beekeeper commands act as its roster entry (`BEEKEEPER_OMP_AGENT` wins over any
session variable its tool shell carries), so it reports its work done with `agents idle --done` like
any worker. `agents handover` refuses omp agents, and no desktop import happens.

A provider's key never lies in a file. omp resolves a provider's `apiKey` in its models file
(`omp.modelsFile`, `~/.omp/agent/models.yml`) as the name of an environment variable first and as
the key itself otherwise, so the file names the variable (`apiKey: SPARK_API_KEY`) and beekeeper's
configuration names where the value lives, an `op://` reference of the shared vault
(`omp.providers.<provider>.apiKey: op://<vault>/<item>/<field>`; a value there is refused when the
configuration loads).

`agents start --harness omp` on a model of such a provider reads the reference through `beekeeper
secret`'s own handling, in the process that holds the vault session (the host's broker with
`secret.session`, so the start runs there as a sandboxed start does; with a service account, the
caller's own), and writes `NAME=value` into the agent's inbox before omp runs: the unit's shell
reads the line, exports the variable and execs omp. No file, no command line and no unit property
carries the value; it lives in omp's environment alone. omp hands its whole environment to the
shells that run the agent's tool commands, so the unit names beekeeper's own bash as `SHELL`
(`<stateDir>/omp/bin/bash`), which drops the variables `BEEKEEPER_OMP_CREDENTIALS` lists before it
runs the command.

A start is refused, before anything is recorded, when the file carries a value for a provider whose
reference the configuration names (the file would hold the key), when it names a variable for a
provider the configuration has no reference for (omp would send the name as the key), when the
configuration names a reference and the file no variable, and when the reference does not answer or
the vault stays locked: nothing starts on a key it does not have.

The start's record and output say which variable the agent got and from which reference
(`$SPARK_API_KEY from op://…`), never the value. A provider the configuration does not name keeps
omp's own key handling.

An omp session the person started in a terminal shows in `sessions` and can be followed, but takes
no messages: omp offers no way into a running interactive session except its collab link, which goes
through omp's relay service.

## agy agents

`agy` (Antigravity CLI) is beekeeper's third local harness. Every `agy` process that runs a session
shows in `sessions`, `snapshot`, `board`, `ui` and `free` with harness `agy`, its state (`busy`
while a turn runs, `idle` while it waits between turns), its title, model, checkout and memory, the
tool commands its shells run, and its last hour from its transcript
(`<agy.brainDir>/<conversation-id>/.system_generated/logs/transcript.jsonl`, default
`~/.gemini/antigravity-cli/brain`). `tail <session>` and `ui` follow it, and `spend` counts the
token usage of every `agy` transcript under `agy.brainDir`.

`agents start <name> <brief> --harness agy --model <m> [--dir <dir>]` requires `--model` (an exact
selector `agy models` lists, such as `claude-opus-4-6-thinking` or `gemini-3-1-pro-medium`; there is
no harness default) and starts `beekeeper agy-run <inbox> <log> agy --input-format stream-json --output-format
stream-json --dangerously-skip-permissions --model <m>` in a transient user unit
`beekeeper-agy-<id[:8]>` in the working directory. The unit carries `BEEKEEPER_AGY_AGENT=<id>` and
`BEEKEEPER_AGENT_NAME=<name>`, and the agent is registered on the roster at once as `agy_<id>`, busy
with `--task`.

Its commands (`agents idle --done --report`, `agents park`, `log add`, `note add`) identify the
session from those environment variables without `--as`. The worker rules and brief go into the
persistent FIFO inbox (`<stateDir>/agy/<id>.in`), and `agents start` waits for the session's stream
log (`<stateDir>/agy/<id>.log`) to record both the `init` frame naming its `conversation_id` and the
`user_input` `step_update` confirming the brief arrived in that conversation; if the unit ends or
fails before confirming the brief, `agents start` stops the unit, marks the roster entry
`undelivered`, and exits non-zero.

When `agy.remoteControl` is true and `agy.remoteDaemon` names a running user unit, `agents start`
restarts that unit after the `<conversation-id>.db` conversation file appears under
`agy.conversationsDir` (unless another running `agy` session is busy mid-turn) so `agy
remote-control` picks up the new conversation.

`agents wake <name> <message>` writes to the agent's inbox. While a turn is running, `beekeeper hook
preinvocation` (configured under `PreInvocation` in `~/.gemini/config/hooks.json` by `beekeeper
install` and checked by `beekeeper doctor`) drains the inbox before the next model call and answers
`injectSteps` with `userMessage` steps so the message lands mid-turn at the next model step rather
than waiting for the turn to end; drained messages stay inflight until the transcript records them
(or the next `user_input`/`result` arrives), and an interrupted hook or crashed process re-delivers
unread messages once without duplicates.

While the session is idle between turns, `beekeeper agy-run` delivers the message as a `stream-json`
user frame on `agy`'s stdin. Taking the agent off the roster (`agents remove`, `beekeeper doctor`)
stops its `beekeeper-agy-<id[:8]>` unit and removes its inbox, queue, lock, stream log and
conversation link files.

## codex agents

`codex` (OpenAI Codex CLI) is beekeeper's fourth local harness. Every `codex` process that runs a
session shows in `sessions`, `snapshot`, `board`, `ui` and `free` with harness `codex`, its state
(`busy` while a turn runs, `idle` while it waits between turns), its title, model, checkout and
memory, the tool commands its shells run, and its last hour from its rollout file under
`<stateDir>/codex/home/sessions` and `codex.sessionsDir`
(`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`). `tail <session>` and `ui` follow it, and `spend`
counts the token usage of every `codex` rollout file.

`agents start <name> <brief> --harness codex [--model m] [--dir d]` resolves the model from
`--model`, else `codex.model` (refused when neither is set), prepares the isolated worker
`CODEX_HOME` at `<stateDir>/codex/home`, and starts `beekeeper codex-run <inbox> <log> <bin>
[model]` in a transient user unit `beekeeper-codex-<id[:8]>` (`KillMode=control-group`) with working
directory `--dir` (default: current directory), `CODEX_HOME=<stateDir>/codex/home`,
`ZDOTDIR=<stateDir>/codex/home/zsh`, `BEEKEEPER_CODEX_AGENT=<id>` and `BEEKEEPER_AGENT_NAME=<name>`.

Because `codex` prefers `OPENAI_API_KEY` over subscription tokens in `auth.json` when both are
present, beekeeper never sets `OPENAI_API_KEY` or `OPENAI_BASE_URL` for subscription workers. The
agent is registered on the roster at once as `codex_<id>`, busy with `--task`, and its beekeeper
commands identify the session from those environment variables without `--as`. The worker rules and
the brief go into the persistent FIFO inbox (`<stateDir>/codex/<id>.in`), and `agents start` waits
for the session's stream log (`<stateDir>/codex/<id>.jsonl`) to record both `thread.started` and
`turn.started` confirming the first turn began; if the unit ends or fails before the first turn
starts, `agents start` stops the unit, removes its inbox and log files, marks the roster entry
`undelivered`, and exits non-zero.

The isolated `CODEX_HOME` (`<stateDir>/codex/home`) links `auth.json` to `codex.authFile`
(`~/.codex/auth.json`) when present, `AGENTS.md` to `~/.codex/AGENTS.md` when present, and `skills`
to `<stateDir>/skills`, and writes `config.toml` and `zsh/.zshenv` + `zsh/.zprofile`.
Only `beekeeper install` manages `hooks.json`; worker starts preserve it and never recreate
it after `beekeeper uninstall`. Uninstall also removes the unmodified legacy worker hook
file created by older worker starts, even without an install manifest.
The `zsh/` shim sets `ZDOTDIR="$HOME"` before sourcing the user's `~/.zshenv` and `~/.zprofile` and
restores `ZDOTDIR` only while a login shell proceeds from `.zshenv` to `.zprofile`, then applies
`agents.shell` (`agents.shell.path`, `agents.shell.unalias`, `setopt nonomatch`), so every `codex`
tool shell (`zsh -lc ...`) and any nested `zsh` subshell keeps the user's dotfiles while putting
`agents.shell.path` first on `PATH`.

`beekeeper codex-run` runs the first turn in the unit's working directory with `codex exec --json --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust
[-m <model>] -` and subsequent turns with `codex exec resume --json --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust
[-m <model>] <thread-id> -`, recording the thread id in `<stateDir>/codex/<id>.thread` and
`<stateDir>/codex/threads/<thread-id>`. `agents wake <name> <message>` writes to the agent's
persistent FIFO inbox (`<stateDir>/codex/<id>.in`), and `codex-run` delivers queued messages through
`hook pretooluse --format codex` as model-visible `additionalContext` at the next tool call inside
the running turn.

The hook preserves guard decisions and consumes only pending wakes, never the runner’s inflight
prompt. If there is no further tool call, delivery waits until the turn ends (whose remaining
duration is unknown); `codex-run` then starts a `codex exec resume` turn with the queued message, or
does so at once when idle. With hooks uninstalled, wakes stay queued until the current turn ends; an
idle runner still starts its next turn immediately. `agents wake` reports when the mid-turn hook is
unavailable and states this fallback explicitly.

Taking the agent off the roster (`agents remove`, `beekeeper doctor`) stops its
`beekeeper-codex-<id[:8]>` unit and removes its inbox, queue, lock, state, stream log and thread
link files.

`capacity.providers.codex` reads the subscription's 5-hour and weekly usage from the freshest
`token_count` event (`event_msg` payload `rate_limits`) across all rollout transcripts in
`<stateDir>/codex/home/sessions` and `codex.sessionsDir` (default command `[codex, usage]`). A newly
started rollout transcript that has written only metadata is skipped when another session holds a
fresh valid reading; an explicit invalid or stopped reading (`spend_control_reached`,
`rate_limit_reached_type`, null or empty `rate_limits`) or a reading older than `staleAfter` fails
closed (`ceiling 0, room 0`).

Once the rollouts' reading is older than `every`, the reader asks the account (`codex app-server`,
`account/rateLimits/read`, no model turn). A request that may pass when sent again (no answer, 429
or 5xx) is sent again after 1 and 3 seconds, within `timeout`; a refusal (401, no login) is not,
and the next refresh asks again. A failed refresh keeps the last reading and says why, with the
endpoint, the status and the error code the account answered, never its body: `account refresh
failed: GET https://chatgpt.com/backend-api/wham/usage answered 401 Unauthorized (token_expired);
using last Codex reading`. `beekeeper capacity` and the watch mark a kept reading older than
`staleAfter` as stale, in the line (`STALE: read 20m ago, max 15m (…)`) and in the JSON (`stale`,
`sourceAt`); the first answer of the account replaces it.

On a Pro plan where only the weekly `primary` window (`window_minutes: 10080`) is reported and
`secondary` is `null`, the 7-day window and weekly pace govern the provider. `capacity.choices`
offers `codex` entries (`{harness: codex, model: <model>}`) to `beekeeper capacity` and `agents
start --harness auto`.

**What a PreToolUse hook cannot cover in Codex 0.165.0:**
- `write_stdin` sends characters to an already-running `exec_command` PTY without firing
  `PreToolUse`. Worker `config.toml` sets `[features] unified_exec_tty = false` so shell commands do
  not open an interactive PTY session, and `<exe> hook pretooluse --format codex` also checks
  `write_stdin` input.
- MCP tools, sub-agent tools (`spawn_agent`/`send_input`), plugins, apps and background daemons do
  not fire `PreToolUse` in Codex 0.165.0. Worker `config.toml` sets `multi_agent = false`,
  `multi_agent_v2 = false`, `plugins = false`, `apps = false` and `daemon_auto_start = false` and
  omits `[mcp_servers]`.
- Because Codex's hook runner treats any non-zero exit status from a `PreToolUse` hook command as
  allow, `beekeeper hook pretooluse --format codex` fails closed (`exit 0` with `permissionDecision:
  "deny"`) on every internal error: malformed or empty stdin, unknown `hook_event_name`, unknown
  `tool_name`, unreadable configuration or state, or an unparseable shell command.
