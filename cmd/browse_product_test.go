package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowseProductDeletionContext(t *testing.T) {
	steps := "My own throwaway agent is named delete-after. I asked you to delete it: click the dialog's Delete agent button once."
	a, _ := stubApp(t)
	rule := productActionRule(steps)
	for _, want := range []string{steps, "explicitly identify the person's own resource", "request this one confirming click", "Verify the page and dialog", "No other resource deletion", "All other rules apply"} {
		if !strings.Contains(rule, want) {
			t.Errorf("rule missing %q", want)
		}
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(browseSettings(turnSelf, []string{rule})), &settings); err != nil {
		t.Fatal(err)
	}
	allow := settings["autoMode"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[0] != autoModeDefaults || allow[1] != rule {
		t.Fatalf("allow rules = %v", allow)
	}
	if productActionRule("Read the agent page") != "" {
		t.Fatal("read-only steps got a deletion rule")
	}
	for _, want := range []string{"whose resource it is", "one confirming click", "classifier error"} {
		if !strings.Contains(a.browseCmd().Long, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

func TestBrowseProductClassifierRefusal(t *testing.T) {
	const classifierError = "classifier error:"
	for _, tc := range []struct{ name, reason, want string }{
		{"category and explanation", "Reason: [Interfere With Workloads]. The requested resource is not identified.", "refused: computer left_click"},
		{"no category", "Reason: [].", classifierError},
		{"whitespace", "Reason: [  ].", classifierError},
		{"no reason", "", classifierError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			use := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"click","name":"mcp__claude-in-chrome__computer","input":{"action":"left_click","action_summary":"Confirms deletion of the test agent"}}]}}`
			text, err := json.Marshal("Permission for this action was " + classifierDenial + ". " + tc.reason)
			if err != nil {
				t.Fatal(err)
			}
			result := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"click","is_error":true,"content":` + string(text) + `}]}}`
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if err := os.WriteFile(path, []byte(use+"\n"+result+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			findings, err := readFindings(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := findings.lines("Delete the test agent", "", nil, false)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], tc.want) || strings.Contains(lines[0], "as []") {
				t.Fatalf("report = %v", lines)
			}
			if tc.name == "category and explanation" && (!strings.Contains(lines[0], "[Interfere With Workloads]") || !strings.Contains(lines[0], "The requested resource is not identified.")) {
				t.Fatalf("lost reason: %v", lines)
			}
		})
	}
}
