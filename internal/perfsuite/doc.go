// Package perfsuite is beekeeper's performance test suite: fixed budgets for
// the CLI calls sessions run most, the supervisor loop's idle pass, concurrent
// calls on the state lock and the per-session footprint, measured on the real
// binary against a generated desk of realistic size with fake GitHub and
// harnesses. The tests carry the perf build tag and run in CI only; see
// docs/performance.md. A local run is opt-in and refused outside a cgroup
// limit (make test-performance-local).
package perfsuite
