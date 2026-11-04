package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditGlobPatternBoundary(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	private := filepath.Join(base, "fixture-private")
	granted := filepath.Join(private, "granted")
	for _, dir := range []string{workspace, private, granted} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h := transcriptHook()
	h.Transcripts.Roots = []string{private}
	h.Transcripts.Grants = map[string][]string{executionFixture: {granted}}
	for _, tc := range []struct {
		name, path, pattern           string
		executionDeny, transcriptDeny bool
	}{
		{"private absolute pattern", workspace, filepath.Join(private, "**", "*.jsonl"), true, true},
		{"private literal pattern", workspace, filepath.Join(private, "record.jsonl"), true, true},
		{"private path", private, "**/*.jsonl", true, true},
		{"workspace absolute pattern", workspace, filepath.Join(workspace, "**", "*.jsonl"), false, false},
		{"workspace relative pattern", workspace, "**/*.jsonl", false, false},
		{"default path relative pattern", "", "**/*.jsonl", false, false},
		{"default path absolute pattern", "", filepath.Join(private, "**", "*.jsonl"), true, true},
		{"granted absolute pattern", workspace, filepath.Join(granted, "**", "*.jsonl"), true, false},
		{"absolute pattern overrides private path", private, filepath.Join(workspace, "**", "*.jsonl"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := executionEvent(globTool, map[string]any{pathKey: tc.path, patternKey: tc.pattern}, workspace)
			if got := executionDenied(h.DecideExecution(raw, executionFixture, workspace, nil)); got != tc.executionDeny {
				t.Errorf("execution denied = %v, want %v", got, tc.executionDeny)
			}
			if got := executionDenied(h.Decide(raw)); got != tc.transcriptDeny {
				t.Errorf("transcript denied = %v, want %v", got, tc.transcriptDeny)
			}
		})
	}
}
