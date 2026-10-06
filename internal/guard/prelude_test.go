package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bash and agentShells are the shells the prelude is tested in.
const bash = "bash"

var agentShells = []string{"zsh", bash}

// The prelude is sourced before a command is parsed, as Claude Code does
// with a session's environment file: the shell's aliases and functions of
// the names are gone and an unmatched glob is a literal, in zsh and bash.
func TestPreludeInAgentShells(t *testing.T) {
	prelude := Prelude([]string{"grep", "ls"}, true, nil, nil)
	setup := "alias ls='echo ALIASED'\ngrep() { echo SHADOWED; }\n"
	for _, sh := range agentShells {
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
			if sh == bash {
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

// The prelude drops every vault credential from the environment, in zsh and
// bash, and leaves the other variables.
func TestPreludeDropsVaultCredentials(t *testing.T) {
	env := filepath.Join(t.TempDir(), "env.sh")
	if err := WritePrelude(env, Prelude(nil, false, nil, nil)); err != nil {
		t.Fatal(err)
	}
	for _, sh := range agentShells {
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed", sh)
			continue
		}
		c := exec.Command(bin, "-c", "source "+env+"\necho \"$?\"; env | cut -d= -f1 | sort") //nolint:gosec // the test's own shells
		c.Env = append(os.Environ(), "OP_SESSION_ABC123=x", "OP_SERVICE_ACCOUNT_TOKEN=x", "OP_CONNECT_TOKEN=x", "OP_ACCOUNT=team")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", sh, err, out)
		}
		got := string(out)
		if !strings.HasPrefix(got, "0\n") || strings.Contains(got, "OP_SESSION_") || strings.Contains(got, "OP_SERVICE_ACCOUNT_TOKEN") ||
			strings.Contains(got, "OP_CONNECT_TOKEN") || !strings.Contains(got, "OP_ACCOUNT\n") {
			t.Errorf("prelude in %s:\n%s", sh, got)
		}
	}
}

// The prelude takes the directories of drop off PATH wherever an inherited
// PATH carries them, once or more, and keeps the rest in order, in zsh and
// bash.
func TestPreludeDropsPath(t *testing.T) {
	env := filepath.Join(t.TempDir(), "env.sh")
	if err := WritePrelude(env, Prelude(nil, false, nil, []string{"/opt/agent bin", "/opt/x"})); err != nil {
		t.Fatal(err)
	}
	for _, sh := range agentShells {
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed", sh)
			continue
		}
		for in, want := range map[string]string{
			"/opt/agent bin:/usr/bin:/opt/x:/bin:/opt/agent bin": "/usr/bin:/bin",
			"/usr/bin:/bin":    "/usr/bin:/bin",
			"/opt/xy:/usr/bin": "/opt/xy:/usr/bin",
		} {
			c := exec.Command(bin, "-c", "PATH='"+in+"'\nsource "+env+"\necho \"$?\"; printf '%s\\n' \"$PATH\"") //nolint:gosec // the test's own shells
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v: %s", sh, err, out)
			}
			if got := string(out); got != "0\n"+want+"\n" {
				t.Errorf("prelude in %s, PATH %q: got %q, want %q", sh, in, got, want)
			}
		}
	}
}

func TestPreludeOff(t *testing.T) {
	if p := Prelude(nil, false, nil, nil); strings.Contains(p, "unalias") || strings.Contains(p, "nomatch") || strings.Contains(p, "PATH") {
		t.Errorf("Prelude(nil, false, nil, nil) = %q", p)
	}
	if p := Prelude(nil, true, nil, nil); strings.Contains(p, "unalias") || !strings.Contains(p, "no_nomatch") {
		t.Errorf("globs only: %q", p)
	}
}

func TestWritePreludeReplacesItsOwnBlockOnly(t *testing.T) {
	env := filepath.Join(t.TempDir(), "env.sh")
	if err := os.WriteFile(env, []byte("export A=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{Prelude([]string{"grep"}, true, nil, nil), Prelude([]string{"ls"}, false, nil, nil), Prelude([]string{"ls"}, false, nil, nil)} {
		if err := WritePrelude(env, p); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(env) //nolint:gosec // the test's own file
	if got, want := string(raw), "export A=1\n"+Prelude([]string{"ls"}, false, nil, nil); got != want {
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

// The prelude's directories go first on PATH in their order, once each, also
// when the environment file is sourced again.
func TestPreludePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	prelude := Prelude(nil, false, []string{"~/agent-bin", "/opt/it's"}, nil)
	for _, sh := range agentShells {
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
			script := "PATH=/usr/bin:/bin\nsource " + env + "\nsource " + env + "\necho \"$?:$PATH\""
			out, err := exec.Command(bin, "-c", script).CombinedOutput() //nolint:gosec // the test's own shells
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got, want := strings.TrimSpace(string(out)), "0:"+home+"/agent-bin:/opt/it's:/usr/bin:/bin"; got != want {
				t.Errorf("PATH in %s = %q, want %q", sh, got, want)
			}
		})
	}
}
