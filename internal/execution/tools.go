package execution

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// ToolGo and ToolGitHub are the worker toolchain a host provisions next to
// its harness: Go for the repositories whose source checkout has a go.mod,
// and the GitHub CLI on every host.
const (
	ToolGo          = "go"
	ToolGitHub      = "gh"
	PinnedToolsFile = "tools.json"
)

// ToolConfig pins one operator-installed tool a worker calls by bare name.
type ToolConfig struct {
	Executable string `yaml:"executable" json:"executable"`
	Version    string `yaml:"version" json:"version"`
}

func (t ToolConfig) validate(name string) error {
	if name != ToolGo && name != ToolGitHub {
		return errors.New("execution host tools must be go or gh")
	}
	if !HostPath(t.Executable) || filepath.Base(t.Executable) != name || !semver.IsValid(goSemver(t.Version)) {
		return fmt.Errorf("tool %s requires an absolute executable named %s and a pinned version", name, name)
	}
	return nil
}

// goSemver turns a Go or gh release ("1.27.3", "1.26", "1.27rc1") into the
// semver form x/mod compares; anything else stays invalid.
func goSemver(v string) string {
	v = strings.TrimPrefix(v, "go")
	if i := strings.IndexAny(v, "abcdefghijklmnopqrstuvwxyz"); i > 0 {
		v = v[:i] + "-" + v[i:]
	}
	return "v" + v
}

// RepositoriesTools names each tool a worker needs for repositories and the
// lowest version that serves it: gh everywhere, Go at the highest toolchain
// (or go line) any repository's go.mod names.
func RepositoriesTools(repositories map[string]string) (map[string]string, error) {
	need := map[string]string{ToolGitHub: ""}
	repos := make([]string, 0, len(repositories))
	for repo := range repositories {
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	for _, repo := range repos {
		p := filepath.Join(repositories[repo], "go.mod")
		b, err := readRegular(p, MaxWire)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading go.mod of %s: %w", repo, err)
		}
		f, err := modfile.Parse(p, b, nil)
		if err != nil {
			return nil, fmt.Errorf("parsing go.mod of %s: %w", repo, err)
		}
		want := ""
		if f.Go != nil {
			want = f.Go.Version
		}
		if f.Toolchain != nil && semver.Compare(goSemver(f.Toolchain.Name), goSemver(want)) > 0 {
			want = strings.TrimPrefix(f.Toolchain.Name, "go")
		}
		if cur, ok := need[ToolGo]; !ok || semver.Compare(goSemver(want), goSemver(cur)) > 0 {
			need[ToolGo] = want
		}
	}
	return need, nil
}

// requiredTools names each tool a worker on this host needs and the lowest
// version that serves it: gh everywhere, Go at the highest toolchain (or go
// line) any served repository's go.mod names.
func (s *Server) requiredTools() (map[string]string, error) {
	return RepositoriesTools(s.Config.Repositories)
}

// CheckTools verifies that the reported host tools satisfy the minimum
// versions required by the served repositories.
func CheckTools(have, need map[string]string) error {
	missing := map[string]string{}
	for name, least := range need {
		v, configured := have[name]
		switch {
		case !configured || v == "":
			missing[name] = "not provisioned under agents.execution.tools"
		case least != "" && semver.Compare(goSemver(v), goSemver(least)) < 0:
			missing[name] = fmt.Sprintf("pinned %s is older than %s the served repositories name", v, least)
		}
	}
	if len(missing) > 0 {
		return toolsError(missing)
	}
	return nil
}

// IsToolsError reports whether err is a worker toolchain readiness refusal.
func IsToolsError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "execution host lacks the worker toolchain (")
}

// RaiseGoTool returns a ToolConfig for version under the same parent tools
// directory as base, or base unchanged when version is not newer.
func RaiseGoTool(base ToolConfig, version string) (ToolConfig, error) {
	if err := base.validate(ToolGo); err != nil {
		return ToolConfig{}, err
	}
	version = strings.TrimPrefix(version, "go")
	if version == "" || !semver.IsValid(goSemver(version)) {
		return ToolConfig{}, fmt.Errorf("invalid go version %q", version)
	}
	if semver.Compare(goSemver(version), goSemver(base.Version)) <= 0 {
		return base, nil
	}
	if filepath.Base(filepath.Dir(base.Executable)) != toolBinDirectory {
		return ToolConfig{}, errors.New("go executable must be under a dedicated installation's bin directory")
	}
	parent := filepath.Dir(filepath.Dir(filepath.Dir(base.Executable)))
	next := ToolConfig{
		Executable: filepath.Join(parent, "go"+version, toolBinDirectory, ToolGo),
		Version:    version,
	}
	if err := next.validate(ToolGo); err != nil {
		return ToolConfig{}, err
	}
	return next, nil
}

// LoadPinnedTools applies a raised Go pin recorded in the host's managed
// execution root when it is newer than the static configuration's pin.
func (s *Server) LoadPinnedTools() error {
	if s.Root == "" || len(s.Config.Tools) == 0 {
		return nil
	}
	base, ok := s.Config.Tools[ToolGo]
	if !ok {
		return nil
	}
	raw, err := readRegular(filepath.Join(s.Root, PinnedToolsFile), MaxWire)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var pinned map[string]ToolConfig
	if err := Decode(raw, &pinned); err != nil {
		return err
	}
	want, ok := pinned[ToolGo]
	if !ok || semver.Compare(goSemver(want.Version), goSemver(base.Version)) <= 0 {
		return nil
	}
	raised, err := RaiseGoTool(base, want.Version)
	if err != nil {
		return err
	}
	if want != raised {
		return errors.New("pinned go tool override does not match configured tools directory")
	}
	tools := maps.Clone(s.Config.Tools)
	tools[ToolGo] = raised
	s.Config.Tools = tools
	return nil
}

// probeTools checks every required tool against its pin: configured, the
// pinned version on the executable's own version line, and at least the
// version the served repositories name. It returns the passing versions and
// one reason per missing tool.
func (s *Server) probeTools(ctx context.Context) (map[string]string, map[string]string, error) {
	if err := s.LoadPinnedTools(); err != nil {
		return nil, nil, err
	}
	need, err := s.requiredTools()
	if err != nil {
		return nil, nil, err
	}
	for name := range s.Config.Tools {
		if _, ok := need[name]; !ok {
			need[name] = ""
		}
	}
	ok, missing := map[string]string{}, map[string]string{}
	for name, least := range need {
		t, configured := s.Config.Tools[name]
		switch {
		case !configured:
			missing[name] = "not provisioned under agents.execution.tools"
		case least != "" && semver.Compare(goSemver(t.Version), goSemver(least)) < 0:
			missing[name] = fmt.Sprintf("pinned %s is older than %s the served repositories name", t.Version, least)
		default:
			if err := s.probeTool(ctx, name, t); err != nil {
				missing[name] = err.Error()
				continue
			}
			ok[name] = t.Version
		}
	}
	return ok, missing, nil
}

func (s *Server) probeTool(ctx context.Context, name string, t ToolConfig) error {
	// GOTOOLCHAIN=local makes go report itself rather than a toolchain a
	// go.mod in the current directory would select and download.
	env := append(cleanEnvironment("", ""), "GOTOOLCHAIN=local")
	args, want := []string{"version"}, "go version go"+t.Version
	if name == ToolGitHub {
		args, want = []string{"--version"}, "gh version "+t.Version
	}
	out, err := s.command(ctx, "", env, t.Executable, args...)
	if err != nil || !strings.HasPrefix(strings.Join(strings.Fields(string(out)), " ")+" ", want+" ") {
		return fmt.Errorf("missing or mismatched %s version at %s", name, t.Executable)
	}
	return nil
}

// GitHubLoggedIn and GitHubLoggedOut are the host's own gh login.
const (
	GitHubLoggedIn  = "logged-in"
	GitHubLoggedOut = "logged-out"
)

// gitHubLogin reads whether the host's gh has a login. gh auth token needs no
// network; what it prints is dropped unread.
func (s *Server) gitHubLogin(ctx context.Context) string {
	if _, err := s.command(ctx, "", cleanEnvironment("", ""), s.Config.Tools[ToolGitHub].Executable, "auth", "token"); err != nil {
		return GitHubLoggedOut
	}
	return GitHubLoggedIn
}

func toolsError(missing map[string]string) error {
	var reasons []string
	for name, reason := range missing {
		reasons = append(reasons, name+": "+reason)
	}
	slices.Sort(reasons)
	return errors.New("execution host lacks the worker toolchain (" + strings.Join(reasons, "; ") + "); install it, pin it under agents.execution.tools and prepare the host again")
}

// toolPath puts the pinned tools' directories on a worker's PATH right after
// the component's, so a worker calls go and gh by bare name.
func (s *Server) toolPath(env []string) []string {
	var dirs []string
	names := make([]string, 0, len(s.Config.Tools))
	for name := range s.Config.Tools {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if d := filepath.Clean(filepath.Dir(s.Config.Tools[name].Executable)); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return env
	}
	out := slices.Clone(env)
	for i, kv := range out {
		v, isPath := strings.CutPrefix(kv, "PATH=")
		if !isPath {
			continue
		}
		list := filepath.SplitList(v)
		var head, rest []string
		if len(list) > 0 && s.Executable != "" && filepath.Clean(list[0]) == filepath.Clean(filepath.Dir(s.Executable)) {
			head, list = list[:1], list[1:]
		}
		for _, d := range list {
			if d != "" && !slices.Contains(dirs, filepath.Clean(d)) {
				rest = append(rest, d)
			}
		}
		out[i] = "PATH=" + strings.Join(slices.Concat(head, dirs, rest), string(filepath.ListSeparator))
		return out
	}
	return append(out, "PATH="+strings.Join(dirs, string(filepath.ListSeparator)))
}

// checkWorkspaceTools checks the committed source, rather than assuming a
// preseeded checkout has the same go.mod as the requested ref. A workspace
// that names a newer Go version than the host's base pin runs with Go's own
// toolchain download into the execution-owned GOPATH/GOMODCACHE.
func (s *Server) checkWorkspaceTools(ctx context.Context, workspace string) error {
	if err := s.LoadPinnedTools(); err != nil {
		return err
	}
	need, err := RepositoriesTools(map[string]string{"workspace/source": workspace})
	if err != nil {
		return err
	}
	if _, ok := need[ToolGo]; ok {
		need[ToolGo] = ""
	}
	missing := map[string]string{}
	for name, least := range need {
		t, configured := s.Config.Tools[name]
		switch {
		case !configured:
			missing[name] = "not provisioned under agents.execution.tools"
		case least != "" && semver.Compare(goSemver(t.Version), goSemver(least)) < 0:
			missing[name] = fmt.Sprintf("pinned %s is older than %s the served repositories name", t.Version, least)
		default:
			if err := s.probeTool(ctx, name, t); err != nil {
				missing[name] = err.Error()
			}
		}
	}
	if len(missing) > 0 {
		return toolsError(missing)
	}
	return nil
}
