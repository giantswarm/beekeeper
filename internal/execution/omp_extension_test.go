package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionOmpExtensionFailsClosed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable for extension contract test")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "guard")
	script := []byte(`#!/bin/sh
case "$1" in
*) cat >/dev/null; cat "$(dirname "$0")/answer";;
esac
`)
	if err := os.WriteFile(bin, script, 0o700); err != nil { //nolint:gosec // executable fake guard in the test's temporary directory
		t.Fatal(err)
	}
	s := Server{Executable: bin, ConfigPath: filepath.Join(dir, "host.yaml")}
	module := filepath.Join(dir, "guard.mjs")
	if err := os.WriteFile(module, s.ompGuardBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `import guard from './guard.mjs';
const handlers = {};
guard({on(name, callback) {handlers[name]=callback;}, setActiveTools(tools) {if (tools.join(',') !== 'bash,read,write') throw new Error('unexpected tool surface');}});
let closed = false;
const ctx = {cwd:'/workspace',sessionManager:{getSessionId(){return 'thread';}},shutdown(){closed=true;}};
const result = await handlers.tool_call({toolName:'bash', input:{command:'git status'}}, ctx);
await handlers.session_start({}, ctx);
console.log(JSON.stringify({blocked:result?.block===true,closed}));
`
	path := filepath.Join(dir, "test.mjs")
	if err := os.WriteFile(path, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, answer string
		blocked      bool
	}{
		{"explicit allow", `{"allow":true}`, false},
		{"deny", `{"allow":false,"reason":"fixture"}`, true},
		{"empty response", "", true},
		{"broken JSON", "{", true},
		{"wrong format", `{"hookSpecificOutput":{}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "answer"), []byte(tc.answer), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command(node, path).CombinedOutput() //nolint:gosec // installed Node runs the test's own fixed fixture script, without a shell
			if err != nil {
				t.Fatalf("extension failed: %v: %s", err, output)
			}
			want := `{"blocked":false,"closed":true}`
			if tc.blocked {
				want = `{"blocked":true,"closed":true}`
			}
			if tc.name == "deny" {
				want = "{\"type\":\"beekeeper_guard_ready\",\"session_id\":\"thread\"}\n{\"blocked\":true,\"closed\":false}"
			}
			if strings.TrimSpace(string(output)) != want {
				t.Fatalf("extension answer %s want %s", output, want)
			}
		})
	}
}
