package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreToolUseGuardsOff(t *testing.T) {
	for _, format := range []string{"claude", "agy", "codex"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			a, dir := scopeApp(t, &out)
			if err := os.WriteFile(a.cfgPath, []byte("stateDir: "+dir+"\nhooks:\n  guards: false\nsandbox:\n  required: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Both secret reads and builds must pass, even when sandbox enforcement
			// would otherwise block this session. Codex's malformed-input guard is
			// deliberately bypassed by the explicit switch as well.
			for _, command := range []string{"sops -d secrets.enc.yaml", "go test ./...", "gh pr merge 1 --repo example/repo"} {
				raw, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": "guards-off", "cwd": filepath.Join(dir, "desk"), "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
				if err != nil {
					t.Fatal(err)
				}
				out.Reset()
				c := a.hookCmd()
				c.SetIn(bytes.NewReader(raw))
				c.SetArgs([]string{"pretooluse", "--format", format})
				if err := c.Execute(); err != nil {
					t.Fatal(err)
				}
				for _, unwanted := range []string{`"deny"`, `"block"`, `updatedInput`, `updated_input`} {
					if strings.Contains(out.String(), unwanted) {
						t.Fatalf("%s: %s", command, out.String())
					}
				}
			}
		})
	}
}
