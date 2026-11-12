package guard

import (
	"encoding/json"
	"testing"
)

// routeOnly feeds a Bash call to RouteBuilds, the hook's answer while
// hooks.guards is off.
func routeOnly(t *testing.T, h Hook, command string, extra map[string]any) *decision {
	t.Helper()
	ti := map[string]any{"command": command}
	for k, v := range extra {
		ti[k] = v
	}
	ev := toolEvent(bashTool, ti)
	ev["cwd"] = "/"
	raw, _ := json.Marshal(ev)
	out := Hook{Self: self, Shell: testShell}.RouteBuilds(raw)
	if out == nil {
		return nil
	}
	var o map[string]decision
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	d := o["hookSpecificOutput"]
	return &d
}

// With the guards off, builds still go to `beekeeper run` (and so to
// run.remote): where a build runs is placement, not a guard.
func TestRouteBuildsWithGuardsOff(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a build alone", "go test ./...", slot("go test ./...")},
		{"only the build of a line", "git fetch && go vet ./... && go test ./...",
			"git fetch && " + slot("go vet ./... && go test ./...")},
		{"cargo", "cargo build --release", slot("cargo build --release")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := routeOnly(t, Hook{}, tc.in, nil)
			if d == nil || d.UpdatedInput == nil {
				t.Fatalf("not routed: %+v", d)
			}
			if got := d.UpdatedInput["command"]; got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
			if d.PermissionDecision != decisionAllow {
				t.Errorf("decision %q, want allow", d.PermissionDecision)
			}
		})
	}
}

func TestRouteBuildsWithGuardsOffLeavesTheRest(t *testing.T) {
	for _, cmd := range []string{
		"git status --short && ls -la",
		"rm -rf /tmp/x",
		self + " run -- zsh -c 'go test ./...'",
		"go version",
		"",
	} {
		if d := routeOnly(t, Hook{}, cmd, nil); d != nil {
			t.Errorf("%q: changed to %+v, want untouched", cmd, d)
		}
	}
	if out := (Hook{}).RouteBuilds([]byte(`{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`)); out != nil {
		t.Errorf("without its own path the hook routes nothing, got %s", out)
	}
	if out := (Hook{Self: self, Shell: testShell}).RouteBuilds([]byte(`{"tool_name":"Write","tool_input":{"file_path":"/x"}}`)); out != nil {
		t.Errorf("a non-Bash tool is untouched, got %s", out)
	}
}

func TestRouteBuildsWithGuardsOffBackground(t *testing.T) {
	d := routeOnly(t, Hook{}, "go test ./...", map[string]any{backgroundKey: true})
	if d == nil || d.UpdatedInput == nil {
		t.Fatalf("not routed: %+v", d)
	}
	if got, want := d.UpdatedInput["command"], backgroundExport+slot("go test ./..."); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
