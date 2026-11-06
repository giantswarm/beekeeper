package guard

import (
	"path/filepath"
	"testing"
)

const recursiveGoGlob = "**/*.go"

func TestGlobPatternsRespectWorkspaceAndTranscriptBoundaries(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(workspace, outside)
	if err != nil {
		t.Fatal(err)
	}
	h := Hook{Transcripts: TranscriptAccess{Roots: []string{outside}, Managed: func(string) (bool, error) { return true, nil }}}
	for name, tc := range map[string]struct {
		path, pattern string
		deny          bool
	}{
		"absolute outside": {workspace, filepath.Join(outside, "**", "*.jsonl"), true},
		"relative outside": {workspace, filepath.Join(relative, "**", "*.jsonl"), true},
		"absolute inside":  {workspace, filepath.Join(workspace, "**", "*.go"), false},
		"relative inside":  {workspace, recursiveGoGlob, false},
		"default path":     {"", recursiveGoGlob, false},
	} {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{pathKey: tc.path, patternKey: tc.pattern}
			raw := executionEvent(globTool, input, workspace)
			if got := executionDenied(h.DecideExecution(raw, executionFixture, workspace, nil)); got != tc.deny {
				t.Errorf("managed execution denied = %v, want %v", got, tc.deny)
			}
			ev := toolEvent(globTool, input)
			ev["cwd"] = workspace
			d := decideEvent(t, h, ev)
			if got := d != nil && d.PermissionDecision == decisionDeny; got != tc.deny {
				t.Errorf("transcript policy denied = %v, want %v", got, tc.deny)
			}
		})
	}
}

func TestRelativeGlobPatternRespectsSandbox(t *testing.T) {
	h, home := sandboxHook(t)
	cwd := filepath.Join(home, "projects", "repo")
	ev := toolEvent(globTool, map[string]any{pathKey: cwd, patternKey: "../../.ssh/*"})
	ev["cwd"] = cwd
	if d := decideEvent(t, h, ev); d == nil || d.PermissionDecision != decisionDeny {
		t.Fatal("a parent-relative Glob pattern escaped the sandbox's readable directories")
	}
}

func TestGlobBracePatternsRespectTranscripts(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := Hook{Transcripts: TranscriptAccess{
		Roots:   []string{filepath.Join(workspace, "private")},
		Managed: func(string) (bool, error) { return true, nil },
	}}
	for _, tc := range []struct {
		pattern string
		deny    bool
	}{
		{"{private,public}/record.jsonl", true},
		{"{private,public}/**/*.jsonl", true},
		{"public/{one,two}/record.jsonl", false},
	} {
		for _, absolute := range []bool{false, true} {
			pattern := tc.pattern
			if absolute {
				pattern = filepath.Join(workspace, pattern)
			}
			t.Run(pattern, func(t *testing.T) {
				ev := toolEvent(globTool, map[string]any{pathKey: workspace, patternKey: pattern})
				ev["cwd"] = workspace
				d := decideEvent(t, h, ev)
				if got := d != nil && d.PermissionDecision == decisionDeny; got != tc.deny {
					t.Errorf("transcript policy denied = %v, want %v", got, tc.deny)
				}
			})
		}
	}
}
