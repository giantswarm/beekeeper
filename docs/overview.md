# How beekeeper works

What beekeeper is for, what it reads and what it keeps. The [documentation index](README.md)
lists every page.

beekeeper keeps the Claude Code sessions sharing one machine working together. When a dozen
sessions run at once on one workstation, they compete for the same things: RAM and swap (one
OOM kill of the desktop scope ends every session), a handful of kind labs and shared
installations, the one browser the Claude in Chrome extension drives, the merges into a
repository that rolls an installation, and the one GitHub REST budget every `gh` and `devctl`
call draws from.

beekeeper reads what is on disk: the process table, the record each Claude Code CLI keeps of the
session it runs (`~/.claude/sessions`), the desktop app's session records, the transcripts and the
git checkouts. It needs no MCP call and no GitHub request to show what every session does. What must
be shared lives in a small state directory that survives restarts: the supervisor, grants, holds,
registered agents, notes, timers and session records. Every session and the supervisor use the same
binary.

Local [session reports](session-reports.md) use a durable supervisor inbox;
read-only status commands read the last atomic snapshot without the state lock.

[`beekeeper transcripts`](transcripts.md) collects every session's transcript across all harnesses
(`claude`, `agy`, `codex`, `omp`) and hosts (the coordinator and SSH execution hosts) into
`<stateDir>/transcripts`, masking secret values from the scan index and credential token rules and
preserving structured dialog answers (`AskUserQuestion`) as searchable user turns across running and
ended sessions. The guide reads and searches all transcripts; a worker reads and searches its own
transcript and the transcripts of workers on the same repository by default, and can request
expiring cross-repository or cross-session access from the guide (`beekeeper transcripts request` /
`beekeeper transcripts grant`).

See [Beekeeper and the desk](desk-boundary.md) for what belongs in the
product and what belongs in a desk's own files, and the doctor's duplicate
workflow warnings.
