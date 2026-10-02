package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// tStarted is a session beekeeper agents start started.
const tStarted = "7a1b2c3d-0000-4000-8000-000000000009"

// scopeApp is an app whose configuration scopes the hooks to <tmp>/desk,
// and the temporary folder.
func scopeApp(t *testing.T, out *bytes.Buffer) (*app, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	conf := "stateDir: " + dir + "\nhooks:\n  scope:\n    dirs: [" + filepath.Join(dir, "desk") + "]\n"
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	return &app{cfgPath: cfg, out: out}, dir
}

// runHook runs `hook <name>` with ev on stdin and returns its answer.
func runHook(t *testing.T, a *app, out *bytes.Buffer, name, ev string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(ev); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	stdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin })
	out.Reset()
	c := a.hookCmd()
	c.SetArgs([]string{name})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func sopsEvent(session, cwd string) string {
	return `{"hook_event_name":"PreToolUse","session_id":"` + session + `","cwd":"` + cwd +
		`","tool_name":"Bash","tool_input":{"command":"sops -d secrets.enc.yaml"}}`
}

// The PreToolUse hook refuses in the scope's directories and in a session
// beekeeper started wherever it runs, and passes anything elsewhere.
func TestHookScope(t *testing.T) {
	var out bytes.Buffer
	a, dir := scopeApp(t, &out)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *state.State) ([]state.Event, error) {
		s.Starts = append(s.Starts, state.Start{Party: state.Party{Session: tStarted}, Mode: state.ModeBypass})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	desk, home := filepath.Join(dir, "desk", "repo"), filepath.Join(dir, "home-project")
	for _, c := range []struct {
		name, session, cwd, project string
		refused                     bool
	}{
		{"desk directory", "s1", desk, "", true},
		{"desk itself", "s1", filepath.Join(dir, "desk"), "", true},
		{"desk project, cwd elsewhere", "s1", home, desk, true},
		{"beekeeper's start elsewhere", tStarted, home, "", true},
		{"own project", "s1", home, home, false},
		{"sibling with the desk's prefix", "s1", filepath.Join(dir, "desk-other"), "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_PROJECT_DIR", c.project)
			got := runHook(t, a, &out, "pretooluse", sopsEvent(c.session, c.cwd))
			if refused := strings.Contains(got, `"deny"`); refused != c.refused {
				t.Errorf("refused = %v, want %v: %q", refused, c.refused, got)
			}
		})
	}
}

// Without hooks.scope.dirs every session is in scope, as before.
func TestHookScopeUnset(t *testing.T) {
	var out bytes.Buffer
	a, _ := permissionApp(t)
	a.out = &out
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if got := runHook(t, a, &out, "pretooluse", sopsEvent("s1", t.TempDir())); !strings.Contains(got, `"deny"`) {
		t.Errorf("unscoped: want the refusal, got %q", got)
	}
}

// Out of scope the PostToolUse hook redacts nothing and the SessionStart
// hook writes no prelude.
func TestHookScopeInertHooks(t *testing.T) {
	var out bytes.Buffer
	a, dir := scopeApp(t, &out)
	home := filepath.Join(dir, "home-project")
	result := `{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"` + home +
		`","tool_name":"Bash","tool_input":{"command":"cat x"},"tool_response":{"stdout":"` + "ghp_" + strings.Repeat("A1b2", 9) + `"}}`
	if got := runHook(t, a, &out, "posttooluse", result); got != "" {
		t.Errorf("posttooluse out of scope answered %q", got)
	}
	if got := runHook(t, a, &out, "posttooluse", strings.Replace(result, home, filepath.Join(dir, "desk"), 1)); !strings.Contains(got, "redacted") {
		t.Errorf("posttooluse in scope: want a redaction, got %q", got)
	}
	envFile := filepath.Join(dir, "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	runHook(t, a, &out, "sessionstart", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"`+home+`"}`)
	if _, err := os.Stat(envFile); err == nil {
		t.Error("sessionstart out of scope wrote the prelude")
	}
	runHook(t, a, &out, "sessionstart", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"`+filepath.Join(dir, "desk")+`"}`)
	if _, err := os.Stat(envFile); err != nil {
		t.Errorf("sessionstart in scope: no prelude: %v", err)
	}
}
