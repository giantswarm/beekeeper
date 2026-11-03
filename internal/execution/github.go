package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	workerGitHubPush        = "push"
	workerGitHubHeadPrefix  = "HEAD:refs/heads/"
	workerGitHubRepoFlag    = "--repo"
	workerGitHubBinDir      = "github-bin"
	workerGitHubClone       = "clone"
	workerGitHubHTTPSPrefix = "https://github.com/"

	GitHubOrigin      = "origin"
	GitHubGitTool     = "git"
	workerGitHubFetch = "fetch"
)

// GitHubBranch is the one upstream branch assigned to this execution.
func GitHubBranch(ref Ref) string {
	return fmt.Sprintf("beekeeper-execution/%s/%d", ref.Execution, ref.Generation)
}

// GitHubEnvironment installs only dispatch scripts, never tokens. The helper
// asks the coordinator on each command; no credential is saved on the host.
func (s *Server) GitHubEnvironment(j Journal, env []string) ([]string, error) {
	if !j.Request.GitHub {
		return env, nil
	}
	dir := filepath.Join(s.Root, workerGitHubBinDir, string(j.Handle.Ref.Execution))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }
	for _, name := range []string{"gh", GitHubGitTool} {
		verb := "github-cli"
		if name == GitHubGitTool {
			verb = "github-git"
		}
		body := []byte("#!/bin/sh\nBEEKEEPER_CONFIG=" + quote(s.ConfigPath) + " exec " + quote(s.Executable) + " agents " + verb + " -- \"$@\"\n")
		path := filepath.Join(dir, name)
		if err := atomicFile(path, body); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // executable dispatch script in the owned component directory
			return nil, err
		}
	}
	out := append([]string{}, env...)
	for i, kv := range out {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			out[i] = "PATH=" + dir + string(filepath.ListSeparator) + v
		}
	}
	return out, nil
}

// RevokeGitHub runs on normal exit and termination, independently of the
// runner's cancelled context. Coordinator stop/inspection also revoke, so
// losing the host cannot keep a worker's credentials alive.
func (s *Server) RevokeGitHub(j Journal) error {
	if !j.Request.GitHub {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	co := s.Config.Coordinators[j.Request.Coordinator]
	var result struct{}
	return CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), "github-revoke", j.Handle.Ref, struct{}{}, j.Request.ControlKey, &result, s.Callback)
}

// GitHubRepository determines the scope of one gh invocation. Cross-owner
// calls must name their repository explicitly; global auth/search commands
// have no safely bounded repository scope.
func GitHubRepository(args []string, fallback string) (string, error) {
	repo := fallback
	for i, arg := range args {
		switch {
		case arg == workerGitHubRepoFlag || arg == "-R":
			if i+1 >= len(args) {
				return "", errors.New("missing GitHub repository")
			}
			repo = args[i+1]
		case strings.HasPrefix(arg, "--repo="):
			repo = strings.TrimPrefix(arg, "--repo=")
		case strings.HasPrefix(arg, workerGitHubHTTPSPrefix):
			parts := strings.Split(strings.TrimPrefix(arg, workerGitHubHTTPSPrefix), "/")
			if len(parts) >= 2 {
				repo = strings.Join(parts[:2], "/")
			}
		case strings.HasPrefix(arg, "repos/"):
			parts := strings.Split(arg, "/")
			if len(parts) >= 3 {
				repo = strings.Join(parts[1:3], "/")
			}
		}
	}
	repo = strings.ToLower(repo)
	if RepositoryIdentity(workerGitHubHTTPSPrefix+repo) != repo {
		return "", errors.New("invalid worker GitHub repository")
	}
	return repo, nil
}

func ValidateWorkerPush(args []string, branch string) error {
	// No implicit refspec, tags, deletion, alternate remote, force or options
	// that can widen the write. Ordinary fetch/status/commit remain regular git.
	if len(args) == 0 || args[0] != workerGitHubPush {
		return nil
	}
	rest := args[1:]
	if len(rest) > 0 && (rest[0] == "-u" || rest[0] == "--set-upstream") {
		rest = rest[1:]
	}
	if len(rest) != 2 || rest[0] != GitHubOrigin || rest[1] != workerGitHubHeadPrefix+branch {
		return errors.New("worker push requires origin HEAD:refs/heads/" + branch)
	}
	return nil
}

// GitHubGitVerb skips git's global options so unauthenticated local commands
// retain normal semantics. Network operations reject such overrides separately.
func GitHubGitVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "-C" || arg == "--git-dir" || arg == "--work-tree" || arg == "--namespace" || arg == "--config-env" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

// GitHubReadRepository scopes explicit read transports to their named repository.
// Clone destinations and fetch refspecs stay client arguments, never authority.
func GitHubReadRepository(args []string) (string, []string, bool) {
	if len(args) == 0 || (args[0] != workerGitHubClone && args[0] != workerGitHubFetch && args[0] != "ls-remote" && args[0] != "pull") {
		return "", args, false
	}
	for i, arg := range args[1:] {
		if repo := RepositoryIdentity(arg); repo != "" {
			normalized := append([]string{}, args...)
			normalized[i+1] = workerGitHubHTTPSPrefix + repo + ".git"
			return repo, normalized, true
		}
	}
	return "", args, false
}
