package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

const (
	shellCodeScript = "shellcode-merge.sh"
	shellCodePython = "shellcode-merge.py"
)

func TestHookRefusesScriptsInShellCode(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	var executable os.FileMode = 0o700 // The ./ script fixtures execute by path.
	for name, text := range map[string]string{
		shellCodeScript: "#!/bin/sh\ndevctl pr merge o/r 7\n",
		shellCodePython: "import os\nos.system('devctl pr merge o/r 7')\n",
		"gated.sh":      self + " gate -- devctl pr merge o/r 7\n",
		"outer.sh":      "bash -c 'bash shellcode-merge.sh'\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), executable); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		cmd, file string
		line      int
	}{
		{`zsh -c 'bash shellcode-merge.sh'`, shellCodeScript, 2},
		{`bash -c 'bash shellcode-merge.sh' ignored -n`, shellCodeScript, 2},
		{`bash shellcode-merge.sh -n`, shellCodeScript, 2},
		{`sh -c '. shellcode-merge.sh'`, shellCodeScript, 2},
		{`bash -c './shellcode-merge.sh'`, shellCodeScript, 2},
		{`bash -lc 'source shellcode-merge.sh'`, shellCodeScript, 2},
		{`bash -cx 'bash shellcode-merge.sh'`, shellCodeScript, 2},
		{`bash -o errexit -c 'bash shellcode-merge.sh'`, shellCodeScript, 2},
		{`bash -c "sh -c 'bash shellcode-merge.sh'"`, shellCodeScript, 2},
		{`bash -c 'python3 shellcode-merge.py'`, shellCodePython, 2},
		{`bash outer.sh`, shellCodeScript, 2},
		{`bash -- shellcode-merge.sh -c true`, shellCodeScript, 2},
		{`bash -c 'cd sub && bash ../shellcode-merge.sh'`, shellCodeScript, 2},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			d := decide(t, h, dir, tc.cmd, nil)
			if d == nil || d.PermissionDecision != decisionDeny {
				t.Fatalf("want refusal, got %+v", d)
			}
			for _, want := range []string{"the script " + filepath.Join(dir, tc.file), fmt.Sprintf("line %d:", tc.line)} {
				if !strings.Contains(d.Reason, want) {
					t.Errorf("reason lacks %q: %s", want, d.Reason)
				}
			}
		})
	}
	for _, cmd := range []string{
		`bash -c 'cat shellcode-merge.sh'`, `bash -c 'echo bash shellcode-merge.sh'`,
		`bash -c 'bash gated.sh'`, `bash -c 'true' shellcode-merge.sh`,
		`bash -c 'bash -n shellcode-merge.sh'`, `bash -n -c 'bash shellcode-merge.sh'`,
		`echo "bash -c 'bash shellcode-merge.sh'"`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if d := decide(t, h, dir, cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
				t.Fatalf("safe command refused: %s", d.Reason)
			}
		})
	}
}

func TestShellCodeAndScriptsShareDepthBound(t *testing.T) {
	h := Hook{Self: self}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, shellCodeScript), []byte("devctl pr merge o/r 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := "bash " + shellCodeScript
	script := code
	for depth := 1; depth <= 6; depth++ {
		code = "bash -c " + ShellQuote(code)
		name := fmt.Sprintf("outer%d.sh", depth)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		script = "bash " + name
		if got, want := h.ungatedRefusal(code, dir) != "", h.ungatedRefusal(script, dir) != ""; got != want || got != (depth <= 4) {
			t.Errorf("depth %d: shell code refused %v, script refused %v", depth, got, want)
		}
	}
}
