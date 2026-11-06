# Real binary supervisor workflow

Run on Linux with Go, Git and util-linux `unshare` installed:

```sh
go test ./internal/testenv -run TestSupervisorWorkflow -count=1 -v
```

The test builds the current candidate from the repository. Two parallel runs
start it through separate supervisor `--once` passes, each with its own config,
state store, board adapter and bare Git origin. The production start path launches
deterministic background workers. They push a real branch, create a pull request
through `gh`, park through Beekeeper, and publish reviews through GitHub's review
endpoint. A stale approval must leave the base unchanged; the exact-head review
must pass the real parser, planner, merge validation and gate. Assertions read the
bare origin's commit and tree, independent durable registrations, durable
review completion, and gate/supervisor completion events.

Only external GitHub and Claude interfaces are replaced. A Git remote helper
connects Git's actual upload/receive-pack protocol to the bare origin while
preserving the production-shaped remote URL. Unknown CLI/API/transport requests
fail. Fixture API state is locked and atomically replaced across subprocesses.
The initial watch budget observation is seeded in the scratch state store;
automatic installation is paused there to keep release discovery out of this
workflow.

Each run enters fresh user, mount, PID and network namespaces. Its filesystem
contains read-only system executables/libraries, a private `/proc`, and its own
writable scratch directories. Host homes, configuration, runtime sockets and
credentials are absent. Child environments use an explicit allowlist. The test
checks that an outbound connection fails and deliberately supplies fake inherited
credentials to verify they do not reach the candidate. Kernel namespace teardown
kills descendants even when the driver is interrupted; an interruption subtest
checks this while the other workflows run. Cleanup only removes that run's
`testing.T.TempDir`.

The test skips with a reason when user/mount/PID/network namespaces are unavailable
or `go test -short` is requested. Existing command-level `loopRig` tests remain
unchanged and cover the planner's smaller cases without this Linux prerequisite.
