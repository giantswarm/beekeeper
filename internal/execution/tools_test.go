package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	fixtureGoExecutable = "/srv/tools/go/bin/go"
	fixtureGoVersion    = "1.27.3"
	fixtureHome         = "HOME=/home/worker"
)

// goFixture gives the fixture's source checkout a go.mod naming toolchain
// and answers go version with installed.
func goFixture(t *testing.T, mod, installed string) fixture {
	t.Helper()
	f := testFixture(t)
	if err := os.WriteFile(filepath.Join(f.source, "go.mod"), []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	next := f.s.Command
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if bin == fixtureGoExecutable {
			if !slices.Contains(env, "GOTOOLCHAIN=local") {
				t.Errorf("go version probe may select a go.mod toolchain: %v", env)
			}
			return []byte("go version go" + installed + " linux/amd64\n"), nil
		}
		return next(ctx, dir, env, bin, args...)
	}
	return f
}

// setTool pins (or, zero, removes) a tool and prepares the changed policy:
// the tools are part of the pinned configuration digest.
func setTool(t *testing.T, f fixture, name string, c ToolConfig) {
	t.Helper()
	if c == (ToolConfig{}) {
		delete(f.s.Config.Tools, name)
	} else {
		f.s.Config.Tools[name] = c
	}
	if _, err := f.s.Prepare(f.s.Policy()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReportsTheHostsGitHubLogin(t *testing.T) {
	for want, answer := range map[string]error{GitHubLoggedIn: nil, GitHubLoggedOut: errors.New("no login")} {
		t.Run(want, func(t *testing.T) {
			f := testFixture(t)
			next := f.s.Command
			f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
				if bin == fixtureGitHubExecutable && slices.Equal(args, []string{"auth", "token"}) {
					return nil, answer
				}
				return next(ctx, dir, env, bin, args...)
			}
			if got := f.s.gitHubLogin(t.Context()); got != want {
				t.Fatalf("login = %q, want %q", got, want)
			}
		})
	}
}

func TestCheckRequiresTheToolchainTheServedRepositoriesName(t *testing.T) {
	ctx := context.Background()
	mod := "module github.com/giantswarm/beekeeper\n\ngo 1.26.0\n\ntoolchain go" + fixtureGoVersion + "\n"
	for _, test := range []struct {
		name, pinned, installed, wantErr string
		tools                            bool
	}{
		{name: "go missing", wantErr: "go: not provisioned under agents.execution.tools"},
		{name: "go older than toolchain", pinned: "1.26.4", installed: "1.26.4", tools: true, wantErr: "go: pinned 1.26.4 is older than " + fixtureGoVersion},
		{name: "installed go differs from pin", pinned: fixtureGoVersion, installed: "1.27.1", tools: true, wantErr: "go: missing or mismatched go version"},
		{name: "go at toolchain", pinned: fixtureGoVersion, installed: fixtureGoVersion, tools: true},
		{name: "newer go", pinned: "1.28.0", installed: "1.28.0", tools: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := goFixture(t, mod, test.installed)
			if test.tools {
				setTool(t, f, ToolGo, ToolConfig{Executable: fixtureGoExecutable, Version: test.pinned})
			}
			caps, err := f.s.Check(ctx)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("check error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !caps.CIWait {
				t.Fatal("prepared host did not advertise its built-in CI wait")
			}
			if caps.Tools[ToolGo] != test.pinned || caps.Tools[ToolGitHub] != fixtureGitHubVersion {
				t.Fatalf("check reported tools %v", caps.Tools)
			}
		})
	}
}

func TestCheckRequiresGitHubCLIEverywhere(t *testing.T) {
	ctx := context.Background()
	f := testFixture(t)
	if caps, err := f.s.Check(ctx); err != nil || caps.Tools[ToolGitHub] != fixtureGitHubVersion || caps.Tools[ToolGo] != "" {
		t.Fatalf("a host without Go repositories needs only gh: %v %v", caps.Tools, err)
	}
	setTool(t, f, ToolGitHub, ToolConfig{})
	if _, err := f.s.Check(ctx); err == nil || !strings.Contains(err.Error(), "gh: not provisioned") {
		t.Fatalf("check without gh = %v", err)
	}
	setTool(t, f, ToolGitHub, ToolConfig{Executable: fixtureGitHubExecutable, Version: "2.80.0"})
	if _, err := f.s.Check(ctx); err == nil || !strings.Contains(err.Error(), "gh: missing or mismatched gh version") {
		t.Fatalf("check with a stale gh pin = %v", err)
	}
	// A start reads the same readiness, so it never launches a worker that
	// could not test or follow its checks.
	if _, err := f.s.Start(ctx, f.r); err == nil || !strings.Contains(err.Error(), "worker toolchain") {
		t.Fatalf("start on a host without its toolchain = %v", err)
	}
}

func TestToolConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		tool string
		t    ToolConfig
		ok   bool
	}{
		{"go", ToolGo, ToolConfig{Executable: fixtureGoExecutable, Version: fixtureGoVersion}, true},
		{"gh", ToolGitHub, ToolConfig{Executable: "/usr/bin/gh", Version: fixtureGitHubVersion}, true},
		{"relative", ToolGo, ToolConfig{Executable: "bin/go", Version: fixtureGoVersion}, false},
		{"renamed", ToolGitHub, ToolConfig{Executable: "/usr/bin/gh-2", Version: fixtureGitHubVersion}, false},
		{"unpinned", ToolGo, ToolConfig{Executable: "/srv/tools/go/bin/go", Version: "latest"}, false},
		{"unknown", "node", ToolConfig{Executable: "/usr/bin/node", Version: "22.0.0"}, false},
	} {
		if err := test.t.validate(test.tool); (err == nil) != test.ok {
			t.Errorf("%s: validate = %v, want ok %v", test.name, err, test.ok)
		}
	}
}

func TestToolPathFollowsTheComponent(t *testing.T) {
	s := &Server{Executable: "/opt/beekeeper/bin/beekeeper", Config: HostConfig{Tools: map[string]ToolConfig{
		ToolGo:     {Executable: fixtureGoExecutable, Version: fixtureGoVersion},
		ToolGitHub: {Executable: "/usr/bin/gh", Version: fixtureGitHubVersion},
	}}}
	got := s.toolPath([]string{fixtureHome, "PATH=/opt/beekeeper/bin:/usr/local/bin:/usr/bin:/srv/tools/go/bin"})
	want := []string{fixtureHome, "PATH=/opt/beekeeper/bin:/usr/bin:/srv/tools/go/bin:/usr/local/bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("toolPath = %v, want %v", got, want)
	}
	if got := s.toolPath([]string{fixtureHome}); !slices.Contains(got, "PATH=/usr/bin:/srv/tools/go/bin") {
		t.Fatalf("toolPath without PATH = %v", got)
	}
	if got := (&Server{}).toolPath([]string{"PATH=/usr/bin"}); !slices.Equal(got, []string{"PATH=/usr/bin"}) {
		t.Fatalf("toolPath without tools = %v", got)
	}
}

func TestWorkspaceChecksActualGoRequirement(t *testing.T) {
	f := testFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example/third\n\ngo 1.26.0\n\ntoolchain go1.27.3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.checkWorkspaceTools(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "go: not provisioned") {
		t.Fatalf("workspace without Go: %v", err)
	}
	// A module file must never follow a link out of the source tree.
	linkDir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "go.mod"), filepath.Join(linkDir, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.checkWorkspaceTools(context.Background(), linkDir); err == nil {
		t.Fatal("linked go.mod accepted")
	}
}
