//go:build darwin

package platform

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
)

func TestSupervisorLoopLaunchAgent(t *testing.T) {
	var spec SetupSpec
	if err := json.Unmarshal([]byte(`{"Home":"/home","Exe":"/opt/my & bin/beekeeper","ConfigPath":"/config/my <desk>.yaml","SupervisorLoop":true}`), &spec); err != nil {
		t.Fatal(err)
	}
	files, _ := (launchdSetup{}).Files(spec)
	for _, f := range files {
		if !strings.HasSuffix(f.Path, "/com.giantswarm.beekeeper.supervisor-loop.plist") {
			continue
		}
		var parsed anyXML
		if err := xml.Unmarshal(f.Content, &parsed); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"/opt/my &amp; bin/beekeeper", "/config/my &lt;desk&gt;.yaml", "<string>supervisor</string><string>loop</string>", "<key>RunAtLoad</key><true/>", "<key>SuccessfulExit</key><false/>"} {
			if !strings.Contains(string(f.Content), want) {
				t.Errorf("launch agent missing %q", want)
			}
		}
		if !f.Service {
			t.Fatal("loop is not marked as a service")
		}
		return
	}
	t.Fatal("opt-in supervisor loop launch agent missing")
}

type anyXML struct{ XMLName xml.Name }
