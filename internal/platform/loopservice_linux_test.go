//go:build linux && !nosystemd

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSupervisorLoopSystemdUnit(t *testing.T) {
	// The loop service is no option: a spec without any loop setting has it.
	var spec SetupSpec
	if err := json.Unmarshal([]byte(`{"ConfigDir":"/config","Exe":"/opt/my bin/bee%keeper","ConfigPath":"/config/my $desk%.yaml"}`), &spec); err != nil {
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
	t.Fatal("the supervisor loop systemd unit is missing: install always writes it")
}
