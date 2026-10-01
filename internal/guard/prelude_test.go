package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The prelude is sourced before a command is parsed, as Claude Code does
// with a session's environment file: the shell's aliases and functions of
// the names are gone and an unmatched glob is a literal, in zsh and bash.
func TestPreludeInAgentShells(t *testing.T) {
	prelude := Prelude([]string{"grep", "ls"}, true)
	setup := "alias ls='echo ALIASED'\ngrep() { echo SHADOWED; }\n"
	for _, sh := range []string{"zsh", "bash"} {
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed", sh)
			continue
		}
		t.Run(sh, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), "env.sh")
			if err := WritePrelude(env, prelude); err != nil {
				t.Fatal(err)
			}
			// What Claude Code runs: the shell's setup, the environment
			// file, then the command, parsed only once the file ran.
			script := setup + "source " + env + "\n" + `eval 'echo "$?"; type ls grep; echo /nonexistent/*.x'`
			args := []string{"-c", script}
			if sh == "bash" {
				args = []string{"-O", "expand_aliases", "-O", "failglob", "-c", script}
			}
			out, err := exec.Command(bin, args...).CombinedOutput() //nolint:gosec // the test's own shells
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			got := string(out)
			if !strings.HasPrefix(got, "0\n") || strings.Contains(got, "ALIASED") || strings.Contains(got, "function") ||
				!strings.Contains(got, "/nonexistent/*.x") {
				t.Errorf("prelude in %s:\n%s", sh, got)
			}
		})
	}
}

func TestPreludeOff(t *testing.T) {
	if p := Prelude(nil, false); p != "" {
		t.Errorf("Prelude(nil, false) = %q", p)
	}
	if p := Prelude(nil, true); strings.Contains(p, "unalias") || !strings.Contains(p, "no_nomatch") {
		t.Errorf("globs only: %q", p)
	}
}

func TestWritePreludeReplacesItsOwnBlockOnly(t *testing.T) {
	env := filepath.Join(t.TempDir(), "env.sh")
	if err := os.WriteFile(env, []byte("export A=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{Prelude([]string{"grep"}, true), Prelude([]string{"ls"}, false), Prelude([]string{"ls"}, false)} {
		if err := WritePrelude(env, p); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(env) //nolint:gosec // the test's own file
	if got, want := string(raw), "export A=1\n"+Prelude([]string{"ls"}, false); got != want {
		t.Errorf("env file:\n%s\nwant:\n%s", got, want)
	}
	if err := WritePrelude(env, ""); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(env) //nolint:gosec // the test's own file
	if string(raw) != "export A=1\n" {
		t.Errorf("prelude off: %q", raw)
	}
}
