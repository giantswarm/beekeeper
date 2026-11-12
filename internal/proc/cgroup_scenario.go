//go:build scenario

package proc

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// ScenarioUnitEnv names, in a behavioral scenario (docs/scenarios.md), the
// unit a process runs in. A scenario's sandbox has no service manager and
// can create no cgroup, so the scenario's own service manager starts each
// unit's process with this variable, which the unit's processes inherit as
// they would inherit its cgroup. Only a build with the scenario tag reads
// it: a release build has no such override.
const ScenarioUnitEnv = "BEEKEEPER_SCENARIO_UNIT"

// scenarioSlice is the cgroup the scenario's units sit in.
const scenarioSlice = "/scenario.slice/"

// scenarioCgroup is the cgroup of pid as the scenario's service manager
// named it: the unit in the process's environment.
func scenarioCgroup(pid int) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", false
	}
	for _, kv := range bytes.Split(raw, []byte{0}) {
		if unit, ok := bytes.CutPrefix(kv, []byte(ScenarioUnitEnv+"=")); ok && len(unit) > 0 {
			return scenarioSlice + string(unit), true
		}
	}
	return "", false
}
