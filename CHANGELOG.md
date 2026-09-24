# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `sessions` and `tail`: every running Claude Code session from disk (processes, desktop records, transcripts, checkouts), what it is on, what it runs, overlaps.
- `snapshot` and `watch`: the machine tick and the Monitor source, with every OOM kill attributed, session starts, ends and restarts, stale leases, and configured checks.
- `budget`: the GitHub budget from a conditional request's headers, with every `gh` and `devctl` process and its session.
- `lease`, `hold`, `supervisor`, `agents`, `note`, `handover`, `log`: shared resources and the supervisor's state on disk, with grants required while a supervisor runs.
- `run` and `hook pretooluse`: a build, test or lint command in one of memcap's build slots under a memory cap (exit 75 when the wait runs out, 137 with the victim when the cap fires), and the PreToolUse hook that routes heavy commands through it and refuses a third kind cluster with the held leases.
- `free [--apply] [--only …] [--summary]`: where the memory is, and what can be freed without touching running work (dead sessions' dirs in the tmpfs `/tmp`, throwaway temp dirs, orphaned workers), with the `free-memory` script's TSV summary for its desktop front end; a live session's dirs stay however idle it is, and what needs root is printed as the command to run.
- `self-update [--check]`: install the latest release once its Sigstore bundle verifies, with one rename that leaves a running `watch` alone; `--check` exits 125 while a newer release is out.

### Fixed

- `hook pretooluse`'s third-lab refusal names each lease holder as `lease list` does (the claim's or the live session's name), no longer the `user@host` holder field.
- `hook pretooluse` leaves a command alone only when it invokes `memcap` or `beekeeper run` at a command position; a wrapper path it merely mentions (`ls …/memcap`, `M=…/memcap`, `grep -v memcap`) no longer lets its build run without a slot.

[Unreleased]: https://github.com/giantswarm/beekeeper/tree/main
