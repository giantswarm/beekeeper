package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

func TestLabConfigLease(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "agentlab.yaml")
	if err := os.WriteFile(config, []byte("clusterName: test-lab\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	holder := lease.Holder{Env: "lab-1", Name: "lab-holder", Session: ownerKey, HostSession: "local_desktop", LabConfig: &lease.LabConfig{Path: config}}
	for _, tc := range []struct {
		name, tool string
		input      map[string]any
		cwd        string
		deny       bool
	}{
		{verbEdit, editTool, map[string]any{filePathKey: config}, dir, true},
		{ompWriteTool, writeTool, map[string]any{filePathKey: "agentlab.yaml"}, dir, true},
		{"symlink", editTool, map[string]any{filePathKey: filepath.Join(alias, "agentlab.yaml")}, dir, true},
		{"redirect", bashTool, map[string]any{commandKey: "echo x > agentlab.yaml"}, dir, true},
		{"cd", bashTool, map[string]any{commandKey: "cd " + ShellQuote(dir) + " && echo x >> agentlab.yaml"}, t.TempDir(), true},
		{"tee", bashTool, map[string]any{commandKey: "echo x | tee agentlab.yaml"}, dir, true},
		{"configure", bashTool, map[string]any{commandKey: "agentlab configure"}, dir, true},
		{"selector", bashTool, map[string]any{commandKey: "agentlab configure --lab test-lab"}, t.TempDir(), true},
		{"move", bashTool, map[string]any{commandKey: "mv replacement agentlab.yaml"}, dir, true},
		{"sed", bashTool, map[string]any{commandKey: "sed -i s/a/b/ agentlab.yaml"}, dir, true},
		{ompReadTool, bashTool, map[string]any{commandKey: "cat agentlab.yaml"}, dir, false},
		{"help", bashTool, map[string]any{commandKey: "agentlab configure --help"}, dir, false},
		{"free lab", writeTool, map[string]any{filePathKey: filepath.Join(t.TempDir(), "agentlab.yaml")}, dir, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, session := range []string{litOther, ownerKey, "desktop", ""} {
				h := Hook{Labs: func() []lease.Holder { return []lease.Holder{holder} }}
				ev := toolEvent(tc.tool, tc.input)
				ev["cwd"] = tc.cwd
				ev["session_id"] = session
				raw, err := json.Marshal(ev)
				if err != nil {
					t.Fatal(err)
				}
				out := string(h.Decide(raw))
				want := tc.deny && session != "owner" && session != "desktop"
				if strings.Contains(out, `"permissionDecision":"deny"`) != want {
					t.Fatalf("session %q: %s; want deny %v", session, out, want)
				}
				if want && !strings.Contains(out, "lab-holder") {
					t.Fatalf("missing holder: %s", out)
				}
			}
		})
	}
}

func TestLabConfigLeaseCodexPatch(t *testing.T) {
	dir := t.TempDir()
	holder := lease.Holder{Env: "lab-1", Name: "lab-holder", Session: "owner", LabConfig: &lease.LabConfig{Path: filepath.Join(dir, "agentlab.yaml")}}
	h := Hook{Labs: func() []lease.Holder { return []lease.Holder{holder} }}
	for _, header := range []string{"Add File", "Update File", "Delete File", "Move to"} {
		for _, session := range []string{litOther, "owner"} {
			patch := "*** Begin Patch\n*** " + header + ": agentlab.yaml\n*** End Patch\n"
			reason, _ := h.checkCodexApplyPatch(event{Session: session}, patch, dir)
			if (reason != "") != (session == "other") {
				t.Fatalf("%s session %s: %s", header, session, reason)
			}
			if session == "other" && !strings.Contains(reason, "lab-holder") {
				t.Fatal(reason)
			}
		}
	}
}
