package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/merge"
)

func TestPreToolUseGuardsOff(t *testing.T) {
	t.Cleanup(func() { github.SetDevctlSkipVersionCheck(false) })
	for _, format := range []string{claudeComm, capacity.HarnessAgy, transcriptTestCodex} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			a, dir := scopeApp(t, &out)
			if err := os.WriteFile(a.cfgPath, []byte("stateDir: "+dir+"\nhooks:\n  guards: false\nsandbox:\n  required: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Secret reads, gh merges and devctl commands must pass unchanged
			// without command rewriting, even when sandbox enforcement would
			// otherwise block this session. A build is never refused either, but
			// a Claude session's build still runs under `beekeeper run` (run.remote):
			// where it runs is placement, not a guard (#3873).
			const build = "go test ./..."
			for _, command := range []string{"sops -d secrets.enc.yaml", build, "gh pr merge 1 --repo example/repo", "devctl pr merge example/repo 1"} {
				routed := command == build && format == claudeComm
				raw, err := json.Marshal(map[string]any{transcriptHookEventKey: guard.PreToolUseEvent, executionOmpSession: "guards-off", executionOmpCwd: filepath.Join(dir, "desk"), transcriptToolName: tToolBash, transcriptInputKey: map[string]string{hookTypeCommand: command}})
				if err != nil {
					t.Fatal(err)
				}
				out.Reset()
				c := a.hookCmd()
				c.SetIn(bytes.NewReader(raw))
				c.SetArgs([]string{configPreToolUse, "--format=" + format})
				if err := c.Execute(); err != nil {
					t.Fatal(err)
				}
				unwanted := []string{`"deny"`, `"block"`, github.DevctlSkipVersionCheckEnv}
				if !routed {
					unwanted = append(unwanted, `updatedInput`, `updated_input`)
				} else if !strings.Contains(out.String(), `updatedInput`) || !strings.Contains(out.String(), ` run -- `) {
					t.Fatalf("%s: not routed through beekeeper run: %s", command, out.String())
				}
				for _, u := range unwanted {
					if strings.Contains(out.String(), u) {
						t.Fatalf("%s: %s", command, out.String())
					}
				}
			}
		})
	}
}

// With the guards off a build is still routed in the hooks' scope, and a
// session on the person's own projects gets no rewrite, as with them on.
func TestPreToolUseGuardsOffRoutesInScope(t *testing.T) {
	var out bytes.Buffer
	a, dir := scopeApp(t, &out)
	desk := filepath.Join(dir, "desk")
	if err := os.WriteFile(a.cfgPath, []byte("stateDir: "+dir+"\nhooks:\n  guards: false\n  scope:\n    dirs: ["+desk+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(cwd string) string {
		return `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"` + cwd +
			`","tool_name":"Bash","tool_input":{"command":"go test ./..."}}`
	}
	if got := runHook(t, a, &out, configPreToolUse, build(filepath.Join(desk, "repo"))); !strings.Contains(got, " run -- ") {
		t.Errorf("in scope: want the build under beekeeper run, got %q", got)
	}
	if got := runHook(t, a, &out, configPreToolUse, build(filepath.Join(dir, "home-project"))); strings.Contains(got, " run -- ") {
		t.Errorf("out of scope: want the build untouched, got %q", got)
	}
}

func TestGuardsOffDevctlVersionGatePropagation(t *testing.T) {
	t.Cleanup(func() { github.SetDevctlSkipVersionCheck(false) })
	const (
		optOut   = github.DevctlSkipVersionCheckEnv + "=true"
		pathEnv  = "PATH=/usr/local/bin"
		repo     = "example/repo"
		ver      = "v1.0.0"
		wantSkip = "SKIP"
		wantEnf  = "ENFORCED"
	)
	var out bytes.Buffer
	a, dir := scopeApp(t, &out)
	// Keep fixture writes independent of configuration loaded by the app.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	a.cfgPath = cfgPath
	envFile := filepath.Join(dir, "claude-env.sh")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	t.Setenv(beekeeperConfigEnv, a.cfgPath)

	writeCfg := func(guards string) {
		t.Helper()
		body := "stateDir: " + dir + "\n"
		if guards != "" {
			body += "hooks:\n  guards: " + guards + "\n"
		}
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil { // #nosec G703 -- test fixture under t.TempDir()
			t.Fatal(err)
		}
	}
	runSessionStart := func() string {
		t.Helper()
		raw, err := json.Marshal(map[string]any{transcriptHookEventKey: "SessionStart", executionOmpSession: "guards-toggle", executionOmpCwd: filepath.Join(dir, "desk")})
		if err != nil {
			t.Fatal(err)
		}
		out.Reset()
		c := a.hookCmd()
		c.SetIn(bytes.NewReader(raw))
		c.SetArgs([]string{"sessionstart"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(envFile) //nolint:gosec // test's own file
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}

	// 1. With hooks.guards: false, sessionstart exports DEVCTL_UNSAFE_SKIP_VERSION_CHECK=true,
	// background cleanEnv includes it, and beekeeper-owned devctl commands receive it.
	writeCfg("false")
	if got := runSessionStart(); !strings.Contains(got, "export "+optOut) {
		t.Fatalf("sessionstart with guards=false did not export %s:\n%s", optOut, got)
	}
	if env := cleanEnv([]string{pathEnv}, a.cfgPath); !slices.Contains(env, optOut) {
		t.Fatalf("cleanEnv with guards=false lacked %s: %v", optOut, env)
	}
	if env := devctlCommandEnv(a.cfgPath); !slices.Contains(env, optOut) {
		t.Fatalf("devctlCommandEnv with guards=false lacked %s: %v", optOut, env)
	}

	bin := t.TempDir()
	fakeDevctl := filepath.Join(bin, merge.Tool)
	script := "#!/bin/sh\nif [ \"${DEVCTL_UNSAFE_SKIP_VERSION_CHECK-}\" = \"true\" ]; then printf '" + wantSkip + "\\n'; else printf '" + wantEnf + "\\n'; fi\n"
	if err := os.WriteFile(fakeDevctl, []byte(script), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx := context.Background()
	if got := strings.TrimSpace(string(gate.ReleaseWait(ctx, repo, ver))); got != wantSkip {
		t.Fatalf("releaseWait with guards=false got %q, want %s", got, wantSkip)
	}
	if raw, err := waitReviewedRelease(ctx, repo, 1); err != nil || strings.TrimSpace(string(raw)) != wantSkip {
		t.Fatalf("waitReviewedRelease with guards=false got %q, %v", raw, err)
	}
	if raw, err := readParkRelease(ctx, repo, ver); err != nil || strings.TrimSpace(string(raw)) != wantSkip {
		t.Fatalf("readParkRelease with guards=false got %q, %v", raw, err)
	}
	if res, err := runExecutionCIRerun(ctx, fakeDevctl, repo, 1); err != nil || strings.TrimSpace(res.Output) != wantSkip {
		t.Fatalf("runExecutionCIRerun with guards=false got %+v, %v", res, err)
	}
	if tok, err := devctlToken(ctx, fakeDevctl); err != nil || tok != wantSkip {
		t.Fatalf("devctlToken with guards=false got %q, %v", tok, err)
	}
	if out, _, code, err := loop.RunArgv(ctx, "", []string{fakeDevctl}); err != nil || code != 0 || strings.TrimSpace(out) != wantSkip {
		t.Fatalf("runArgv with guards=false got %q, code=%d, err=%v", out, code, err)
	}

	// 2. Re-enabling hooks.guards unsets the shell variable and strips it from
	// worker environments and beekeeper-owned devctl child commands even if set
	// in the ambient environment.
	t.Setenv(github.DevctlSkipVersionCheckEnv, "true")
	writeCfg("true")
	if got := runSessionStart(); !strings.Contains(got, "unset "+github.DevctlSkipVersionCheckEnv) || strings.Contains(got, "export "+optOut) {
		t.Fatalf("sessionstart with guards=true did not unset %s:\n%s", github.DevctlSkipVersionCheckEnv, got)
	}
	if env := cleanEnv([]string{pathEnv, optOut}, a.cfgPath); slices.Contains(env, optOut) {
		t.Fatalf("cleanEnv with guards=true kept %s: %v", optOut, env)
	}
	if env := devctlCommandEnv(a.cfgPath); slices.Contains(env, optOut) {
		t.Fatalf("devctlCommandEnv with guards=true kept %s: %v", optOut, env)
	}
	if got := strings.TrimSpace(string(gate.ReleaseWait(ctx, repo, ver))); got != wantEnf {
		t.Fatalf("releaseWait with guards=true got %q, want %s", got, wantEnf)
	}
	if tok, err := devctlToken(ctx, fakeDevctl); err != nil || tok != wantEnf {
		t.Fatalf("devctlToken with guards=true got %q, %v", tok, err)
	}
	if out, _, code, err := loop.RunArgv(ctx, "", []string{fakeDevctl}); err != nil || code != 0 || strings.TrimSpace(out) != wantEnf {
		t.Fatalf("runArgv with guards=true got %q, code=%d, err=%v", out, code, err)
	}
}
