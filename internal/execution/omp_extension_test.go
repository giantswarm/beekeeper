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
	if err := os.WriteFile(bin, []byte(`#!/bin/sh
case "$1" in
*) cat >/dev/null; cat "$(dirname "$0")/answer";;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Server{Executable: bin, ConfigPath: filepath.Join(dir, "host.yaml")}
	module := filepath.Join(dir, "guard.mjs")
	if err := os.WriteFile(module, s.ompGuardBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `import guard from './guard.mjs';
const handlers = {};
guard({on(name, callback) {handlers[name]=callback;}});
const result = await handlers.tool_call({toolName:'bash', input:{command:'git status'}}, {cwd:'/workspace',sessionManager:{getSessionId(){return 'thread';}}});
console.log(JSON.stringify({blocked:result?.block===true}));
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
			output, err := exec.Command(node, path).CombinedOutput()
			if err != nil {
				t.Fatalf("extension failed: %v: %s", err, output)
			}
			want := `{"blocked":false}`
			if tc.blocked {
				want = `{"blocked":true}`
			}
			if strings.TrimSpace(string(output)) != want {
				t.Fatalf("extension answer %s want %s", output, want)
			}
		})
	}
}
