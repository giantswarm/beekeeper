// Package scenario is beekeeper's behavioral scenario harness: scenarios as
// YAML under scenarios/<flow>/, each run against the real candidate binary in
// its own sandboxed desk, with every external program (gh, the harnesses,
// ssh, devctl, systemctl, journalctl) answered by the scenario's world.
//
// The harness carries the scenario build tag and runs with
// make test-scenarios; see docs/scenarios.md. The plan is
// docs/plans/3788-behavioral-scenarios.md.
package scenario
