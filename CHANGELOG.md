# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- The merge gate: the PreToolUse hook puts `beekeeper gate` in front of every `devctl pr merge`. Merges queue per lane (`lanes` in the configuration), one at a time, the next once the lane's HelmReleases are Ready and the previous release rolled; a held repository, lane, `merges` or `github`, a budget under the floor and an unreadable installation refuse (exit 77); a wait past its bound exits 76 and keeps the merge's place; the machine runs at most `merge.cap` devctl processes; a giantswarm/devctl merge opens the tool-release window. devctl's document and exit code pass through unchanged.
- `lanes [queue|drop|clear]`: each lane's running, settling and waiting merges, also in `handover`; `queue <owner/repo> <n> --for <session>` seeds a session's place so an agreed order carries over.
- `hold set|lift|check --lane <name>`, the `merges` target and `--except`.
- `lanes settle <owner/repo> <n> [--for <session>]`: a merge run outside the gate heads its lane until it merges, so its own `devctl pr merge` passes the gate while the lane's other merges wait; merged through the gate or outside it (GitHub asked at most once a minute), it settles the lane like a gated merge. `lanes` and `handover` show it.
- `hold set --except owner/repo#n` on a lane or repository hold: the hold refuses every merge but that pull request's; `hold check` takes `owner/repo#n`, and `hold`, `lanes` and `handover` show the exception.

- `sessions` and `tail`: every running Claude Code session from disk (processes, desktop records, transcripts, checkouts), what it is on, what it runs, overlaps.
- `snapshot` and `watch`: the machine tick and the Monitor source, with every OOM kill attributed, session starts, ends and restarts, stale leases, and the installations' alerts.
- `budget`: the GitHub budget from a conditional request's headers, with every `gh` and `devctl` process and its session.
- `lease`, `hold`, `supervisor`, `agents`, `note`, `handover`, `log`: shared resources and the supervisor's state on disk, with grants required while a supervisor runs.
- `run` and `hook pretooluse`: a build, test or lint command in one of memcap's build slots under a memory cap (exit 75 when the wait runs out, 137 with the victim when the cap fires), and the PreToolUse hook that routes heavy commands through it and refuses a third kind cluster with the held leases.
- `free [--apply] [--only …] [--summary]`: where the memory is, and what can be freed without touching running work (dead sessions' dirs in the tmpfs `/tmp`, throwaway temp dirs, orphaned workers), with the `free-memory` script's TSV summary for its desktop front end; a live session's dirs stay however idle it is, and what needs root is printed as the command to run.
- `alerts watch|snapshot|import` and the `alerts:` configuration: each installation's Alertmanager (Mimir's with the `giantswarm` tenant, else the plain one) read natively over bounded `kubectl port-forward`s, in parallel with a timeout each; NEW and RESOLVED lines with pages and the configured team marked, a burst of one alertname collapsed, one line when an installation stops or starts answering, and the lease holder and sessions on the installation named; `alerts.ignore` for known flappers; one baseline with one owner, and `import` to take over an existing one. `watch` and `snapshot` include them, and `handover` shows what the alert watch reads.
- `self-update [--check]`: install the latest release once its Sigstore bundle verifies, with one rename that leaves a running `watch` alone; `--check` exits 125 while a newer release is out.

### Removed

- The `checks:` configuration and `snapshot --no-checks`: the alerts it ran a script for are read natively (`snapshot --no-alerts` skips them).

### Fixed

- A merged run whose release devctl could not confirm no longer keeps its lane until `merge.settleTimeout`: after `merge.settle` only the lane's HelmReleases decide.

- `watch` stopped by SIGINT or SIGTERM during a poll no longer reports the journal or the GitHub budget as unreadable.
- `hook pretooluse`'s third-lab refusal names each lease holder as `lease list` does (the claim's or the live session's name), no longer the `user@host` holder field.
- `hook pretooluse` leaves a command alone only when it invokes `memcap` or `beekeeper run` at a command position; a wrapper path it merely mentions (`ls …/memcap`, `M=…/memcap`, `grep -v memcap`) no longer lets its build run without a slot.

[Unreleased]: https://github.com/giantswarm/beekeeper/tree/main
