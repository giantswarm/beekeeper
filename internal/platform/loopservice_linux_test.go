//go:build linux && !nosystemd

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSupervisorLoopSystemdUnit(t *testing.T) {
	// JSON also lets this regression run against the pre-service SetupSpec:
	// unknown fields then leave the opt-in service absent.
	var spec SetupSpec
	if err := json.Unmarshal([]byte(`{"ConfigDir":"/config","Exe":"/opt/my bin/bee%keeper","ConfigPath":"/config/my $desk%.yaml","SupervisorLoop":true}`), &spec); err != nil {
		t.Fatal(err)
	}
	files, _ := (systemdSetup{}).Files(spec)
	for _, f := range files {
		if !strings.HasSuffix(f.Path, "/beekeeper-supervisor-loop.service") {
			continue
		}
		for _, want := range []string{`ExecStart=:"/opt/my bin/bee%%keeper" --config "/config/my $desk%%.yaml" supervisor loop`, "Restart=on-failure", "WantedBy=default.target"} {
			if !strings.Contains(string(f.Content), want) {
				t.Errorf("loop unit missing %q:\n%s", want, f.Content)
			}
		}
		if !f.Service {
			t.Fatal("loop is not marked as a service")
		}
		return
	}
	t.Fatal("opt-in supervisor loop systemd unit missing")
}
