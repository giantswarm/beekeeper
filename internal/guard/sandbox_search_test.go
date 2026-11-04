package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxSearchKeepsDeniedDescendantsClosed(t *testing.T) {
	h, home := sandboxHook(t)
	cwd := filepath.Join(home, "projects/repo")
	runtime := filepath.Join(filepath.Dir(home), "runtime")
	h.Sandbox.Runtime = runtime
	state := filepath.Join(home, ".local/state/beekeeper")
	scan := filepath.Join(state, "scan")
	h.Sandbox.Deny = []string{scan}
	for _, path := range []string{filepath.Join(home, "private/canary.txt"), filepath.Join(runtime, "private/canary.txt"), filepath.Join(scan, "index.json")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("public audit canary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{grepTool, globTool} {
		for _, root := range []string{string(filepath.Separator), filepath.Dir(home), filepath.Dir(runtime), state, cwd} {
			wantDeny := root != cwd
			input := map[string]any{pathKey: root, patternKey: "canary", "output_mode": "content"}
			if tool == globTool {
				input[patternKey] = root + "/**/*.txt"
			}
			ev := toolEvent(tool, input)
			ev["cwd"] = cwd
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			out := h.DecideSandbox(raw)
			if denied := strings.Contains(string(out), `"permissionDecision":"deny"`); denied != wantDeny {
				t.Errorf("sandbox-only %s at %s: %s; want denial %v", tool, root, out, wantDeny)
			}
			decision := decideEvent(t, h, ev)
			if denied := decision != nil && decision.PermissionDecision == decisionDeny; denied != wantDeny {
				t.Errorf("normal hook %s at %s: %+v; want denial %v", tool, root, decision, wantDeny)
			}
		}
	}
	// Reading the state directory itself and a safe file remains permitted.
	for _, path := range []string{state, filepath.Join(state, "state.json")} {
		ev := toolEvent(readTool, map[string]any{filePathKey: path})
		ev["cwd"] = cwd
		raw, _ := json.Marshal(ev)
		if out := h.DecideSandbox(raw); len(out) != 0 {
			t.Errorf("ordinary Read %s refused: %s", path, out)
		}
	}
}
