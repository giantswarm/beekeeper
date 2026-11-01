## Problem

A managed SSH execution host with beekeeper installed at an absolute per-user path passed the prerequisite installer check and `agents hosts check`. The worker started and accepted its first task, but its Bash tools saw a PATH containing only `/usr/local/bin:/usr/bin`, and every callback (`agents note`, `agents park`, `agents idle`) failed with `beekeeper: command not found`. The host check had no opinion about whether the embedded worker contract could execute its component at all.

## Cause

The owned runner launches the harness with a cleaned environment that passes the inherited `PATH` through unchanged. The user service manager starts the execution unit with a minimal PATH, so a per-user installation directory was never visible inside the worker, while readiness (`Check`) only validated the policy, guard, harness version and authentication.

## Fix

- `cleanEnvironment` now takes the policy-bound beekeeper executable and places its directory first on the harness `PATH`, dropping any duplicate later entry so a stale copy elsewhere cannot shadow the installation the prepared policy pins. The bare worker commands resolve to exactly the executable the host policy binds, so the accepted installation may be per-user; no global install is required and the task brief no longer needs to carry installation paths.
- `Check` (and therefore `agents hosts check` and every start, which re-checks) refuses a host whose bound installation is missing, not a regular file, or not executable, instead of starting a worker that cannot report or park.

## Evidence

- `TestRunnerPinsWorkerContractToInstalledExecutable`: the harness under the owned runner resolves `beekeeper` to the installed path even with a stale executable first on the inherited PATH — fails on the base commit, passes with the fix.
- `TestCheckRefusesUnresolvableWorkerContract`: readiness and start fail closed on a missing or non-executable bound installation — fails on the base commit, passes with the fix.
- `TestCleanEnvironmentPinsComponentDirectory`: PATH pinning, duplicate dropping, and the empty-PATH case.
- Targeted suites `./internal/execution/` and the execution/cmd suites pass; full suite, lint and platform checks are left to CI.

Closes https://github.com/giantswarm/beekeeper/issues/1802
