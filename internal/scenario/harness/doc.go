// Package harness is the scenario harness's fake machines: the coding agent
// CLIs beekeeper starts (claude, codex, agy, omp), the service manager their
// runners live in (systemd-run, systemctl) and the SSH execution hosts
// (ssh), each a host root inside the scenario's sandbox.
//
// Every fake is the scenario's one fake executable under another name. It
// is a real process, named as the real CLI is, because beekeeper finds its
// sessions in the process table: a fake claude background session is a
// claude process under a claude daemon. What a fake does is decided by the
// scenario's world: it logs every call, hands a worker its script (real
// beekeeper, git and gh calls and a turn outcome), runs the transient
// units and maps a host alias to its root.
//
// The package carries the scenario build tag; see docs/scenarios.md.
package harness
