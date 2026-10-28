package execution

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// RepositoryIdentity accepts the canonical public GitHub transport forms,
// without passwords, query strings or a caller-selected credential helper.
func RepositoryIdentity(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@github.com:") {
		r := strings.TrimSuffix(strings.TrimPrefix(raw, "git@github.com:"), ".git")
		if repoPattern.MatchString(r) {
			return strings.ToLower(r)
		}
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	if u.Scheme == "https" && u.User != nil || u.Scheme != "https" && u.Scheme != sshCommand {
		return ""
	}
	if u.Scheme == sshCommand && (u.User == nil || u.User.Username() != "git") {
		return ""
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return ""
		}
	}
	r := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	if !repoPattern.MatchString(r) {
		return ""
	}
	return strings.ToLower(r)
}

func (s *Server) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// Provisioning operates only on host-local sources. Global filters and
	// checkout hooks must not execute before registration or inherit the
	// operator's unrelated Git settings.
	args = append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false"}, args...)
	return s.command(ctx, dir, append(cleanEnvironment(""), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull), "git", args...)
}

func (s *Server) workspace(ctx context.Context, j *Journal) error {
	source, ok := s.Config.Repositories[j.Request.Repository]
	if !ok {
		return errors.New("repository is not mapped on the execution host")
	}
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return errors.New("execution host checkout is unavailable")
	}
	origin, err := s.git(ctx, source, "remote", "get-url", "origin")
	if err != nil || RepositoryIdentity(string(origin)) != strings.ToLower(j.Request.Repository) {
		return errors.New("execution host checkout origin does not match the repository allowlist")
	}
	root, err := filepath.EvalSymlinks(s.Config.WorkspaceRoot)
	if err != nil || root != filepath.Clean(s.Config.WorkspaceRoot) {
		return errors.New("workspace root is missing or resolves through a symlink")
	}
	parent := filepath.Join(root, string(j.Request.Ref.Execution))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent || !within(root, resolved) {
		return errors.New("workspace escapes its execution root")
	}
	workspace := filepath.Join(parent, "workspace")
	if j.Handle.Workspace != "" && j.Handle.Workspace != workspace {
		return errors.New("workspace identity mismatch")
	}
	j.Handle.Workspace = workspace
	if _, err := os.Lstat(workspace); err == nil {
		if j.Handle.Commit == "" {
			return errors.New("interrupted clone preserved; inspect this preparation before creating another execution")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		if _, err := s.git(ctx, "", "clone", "--no-local", "--no-hardlinks", "--", source, workspace); err != nil {
			return errors.New("isolated workspace clone failed; partial work is preserved")
		}
		if _, err := s.git(ctx, workspace, "remote", "set-url", "origin", strings.TrimSpace(string(origin))); err != nil {
			return errors.New("cannot establish workspace repository identity")
		}
		if _, err := s.git(ctx, workspace, "fetch", "--no-tags", "--", source, j.Request.GitRef); err != nil {
			return errors.New("cannot resolve the requested ref on the execution host")
		}
		commit, err := s.git(ctx, workspace, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
		if err != nil {
			return errors.New("requested ref is not a commit")
		}
		j.Handle.Commit = strings.TrimSpace(string(commit))
		p, err := s.journalPath(j.Handle.Ref)
		if err != nil {
			return err
		}
		if err := atomicFile(p, mustJSON(j)); err != nil {
			return err
		}
	}
	resolved, err = filepath.EvalSymlinks(workspace)
	if err != nil || resolved != workspace {
		return errors.New("workspace is not an isolated directory")
	}
	common, err := s.git(ctx, workspace, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || strings.TrimSpace(string(common)) != filepath.Join(workspace, ".git") {
		return errors.New("workspace shares writable Git metadata")
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("workspace borrows another repository's object store")
	}
	status, err := s.git(ctx, workspace, "status", "--porcelain", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return errors.New("workspace has uncommitted work; it is preserved")
	}
	if _, err := s.git(ctx, workspace, "checkout", "--detach", j.Handle.Commit, "--"); err != nil {
		return errors.New("cannot check out the pinned commit without changing existing work")
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".beekeeper-scratch"), 0o700); err != nil {
		return err
	}
	exclude := filepath.Join(workspace, ".git", "info", "exclude")
	contents, err := readRegular(exclude, MaxWire)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !strings.Contains(string(contents), "\n/.beekeeper-scratch/\n") {
		if err := atomicFile(exclude, append(contents, []byte("\n/.beekeeper-scratch/\n")...)); err != nil {
			return err
		}
	}
	// The harness loads the same embedded rules independently of project
	// instructions, including repositories that supply no CLAUDE.md.
	return nil
}
