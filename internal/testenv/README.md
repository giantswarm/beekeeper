# Test isolation and the scenario sandbox

`Setup` isolates a test binary from the desk that starts it: it clears the
`BEEKEEPER_*`, `CLAUDE_*`, `CODEX_*` and `GIT_*` variables (except
`BEEKEEPER_TEST_*`) and gives Git a configuration of its own.

`EnterSandbox` is the sandbox every behavioral scenario runs in
([docs/scenarios.md](../../docs/scenarios.md)). The caller is already in
fresh user, mount, PID and network namespaces (`unshare -Urmpnf`); the
sandbox then holds read-only system executables and libraries, a private
`/proc`, the devices Git needs and the run's own writable `/work`. Host homes,
configuration, runtime sockets and the network are absent. Kernel namespace
teardown kills every descendant even when the driver is interrupted.

`TestSandbox` checks this from inside (no outbound connection, no `/home`,
`/usr` read-only, `/work` writable) and checks that an interrupted run leaves
no process behind and touches no other run. It needs Linux with unprivileged
namespaces and skips without them; `make test-scenarios` runs it with the
scenarios:

```sh
go test ./internal/testenv -run TestSandbox -count=1 -v
```

The real-binary supervisor workflow that lived here moved into scenarios
(https://github.com/giantswarm/beekeeper/issues/3797).
