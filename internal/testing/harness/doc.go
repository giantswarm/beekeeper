// Package harness runs the phase 1 scenarios (docs/phase1/scenarios.md §3, §4) against the real engine,
// in-process: a real kube-apiserver from k8senv, a real Postgres, the real receiver, forge client and
// history writer, and in-process fakes for GitHub and JEV, all on one fake clock per scenario.
//
// The pieces:
//   - LoadDir, Parse: the strict loader (unknown keys, a missing description, an unregistered tool or
//     argument, an unknown event type, metric or invariant all fail the load) and the fixture merge.
//   - Catalogue: CATALOGUE.md, written by go run ./internal/testing/scenariolint.
//   - Expected.Evaluate: the one expectation evaluator for every tool.
//   - World: one scenario's running system; NewWorld builds it, Run runs the steps and the post-checks.
//   - The step tools, registered by name with their arguments (Register, Lookup).
//
// Running (a laptop runs 2 scenarios at once on 2 API servers; CI goes wider with -parallel 8
// -scenario.pool 4, or BEEKEEPER_SCENARIO_POOL):
//
//	go test ./internal/testing/harness/ -run TestScenarios -parallel 2          # all
//	go test ./internal/testing/harness/ -run TestScenarios/wait-ci-wakes-on-check-run
//	go test ./internal/testing/harness/ -run TestScenarios -scenario.concept wait
//	go test ./internal/testing/harness/ -run TestScenarios -scenario.tag learning-api-budget
//	go test ./internal/testing/harness/ -run TestScenarios -v                   # timings, over-budget list
//	go test ./internal/testing/harness/ -run TestScenarios/X -v -scenario.phases -scenario.logs
//
// A failing scenario reports the failed step (its args, every unmet expectation and the result), the
// post-checks and the tail of the engine's logs; a settle that times out names what was still busy and
// writes a goroutine dump to ~/.cache/beekeeper/scenario-dumps. -v also lists the failures by concept.
//
// The API server, kine and Postgres binaries come from $BEEKEEPER_TEST_DEPS (default
// ~/.cache/beekeeper/test-deps, filled by make test-deps); without them the scenarios are skipped.
package harness
