package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// The helper contains no credential; it expands the client-only environment.
//
//nolint:gosec // G101: a credential helper template, not a hardcoded token
const workerGitHubCredentialHelper = `credential.https://github.com.helper=!f() { printf 'username=x-access-token\npassword=%s\n' "$GH_TOKEN"; }; f`

const workerGitHubHead = "HEAD"
const workerGitHubHeadFlag = "--head"
const workerGitHubNoCredentialHelper = "credential.helper=" //nolint:gosec // G101: clears inherited Git helpers; contains no credential

// SSH execution hosts run Linux. Resolve git outside the worker-controlled
// PATH; the test seam substitutes a fixture executable, never a runtime setting.
var workerGitHubGitExecutable = "/usr/bin/git"

func (a *app) executionGitHubCommands() []*cobra.Command {
	commands := []*cobra.Command{}
	for _, name := range []string{"github-cli", "github-git"} {
		name := name
		commands = append(commands, &cobra.Command{Use: name, Hidden: true, DisableFlagParsing: true, RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
			s, j, err := a.executionCaller()
			if err != nil {
				return err
			}
			if !j.Request.GitHub {
				return refused("worker GitHub access is not enabled")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Hour)
			defer cancel()
			co := s.Config.Coordinators[j.Request.Coordinator]
			workDir, defaultRepo := workerGitHubWorkDir(j.Handle.Workspace, j.Request.Repository)
			request := workerGitHubRequest{Repository: defaultRepo}
			env := os.Environ()
			var binary string
			needsToken := true
			targetBranch := executionGitHubTargetBranch(j)
			if name == "github-cli" {
				if len(args) == 0 {
					return refused("worker gh requires a repository command")
				}
				switch args[0] {
				case workerGitHubIssueGroup, "pr", workerGitHubAPIGroup, workerGitHubRepoGroup, workerGitHubRunGroup, workerGitHubWorkflowGroup, workerGitHubReleaseGroup:
				default:
					return refused("worker gh requires an explicit repository command; auth, aliases and global command overrides are unavailable")
				}
				if len(args) > 1 && args[0] == "pr" && args[1] == gitMerge {
					return refused("merges stay on the coordinator")
				}
				if args[0] == workerGitHubAPIGroup {
					err = execution.ValidateGitHubAPIRead(args[1:])
				} else {
					request.Write, err = execution.GitHubWrite(args)
				}
				if err != nil {
					return refused("%v", err)
				}
				if request.Write {
					if request.Repository, err = execution.GitHubWriteRepository(args, request.Repository); err != nil {
						return refused("%v", err)
					}
				} else if request.Repository, err = execution.GitHubRepository(args, request.Repository); err != nil {
					return err
				}
				if args[0] == "pr" && len(args) > 1 && args[1] == workerGitHubCreate {
					branch := execution.GitHubBranch(j.Handle.Ref)
					for i, arg := range args {
						if arg == workerGitHubHeadFlag || arg == "-H" {
							if i+1 >= len(args) || args[i+1] != branch {
								return refused("PR must use its execution branch")
							}
						}
						if strings.HasPrefix(arg, "--head=") && arg != "--head="+branch {
							return refused("PR must use its execution branch")
						}
					}
					args = append(args, workerGitHubHeadFlag, branch)
				}
				binary = s.Config.Tools[execution.ToolGitHub].Executable
			} else {
				if execution.GitHubGitVerb(args) == workerGitHubPush {
					resolve := workerGitHubRequest{Repository: defaultRepo, Write: true, ResolvePush: true}
					var target workerGitHubPushTarget
					if err := execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), workerGitHubIssue, j.Handle.Ref, resolve, j.Request.ControlKey, &target, s.Callback); err != nil {
						return err
					}
					if target.Branch == "" {
						return refused("coordinator returned no push branch")
					}
					targetBranch = target.Branch
					request.PushBranch = targetBranch
				}
				if err := execution.ValidateWorkerPush(args, targetBranch); err != nil {
					return refused("%v", err)
				}
				binary = workerGitHubGitExecutable

				if binary == "" {
					return errors.New("host git executable unavailable")
				}
				verb := execution.GitHubGitVerb(args)
				request.Write = verb == workerGitHubPush
				needsToken = verb == workerGitHubPush || verb == "fetch" || verb == "pull" || verb == "ls-remote"
				if needsToken && (len(args) == 0 || args[0] != verb) {
					return refused("network git cannot override the owned workspace or configuration")
				}
				if request.Write {
					if err := execution.ValidateWorkerPush(args, targetBranch); err != nil {
						return err
					}
				}

				if !request.Write {
					if repo, normalized, scoped := execution.GitHubReadRepository(args); scoped {
						request.Repository, args, needsToken = repo, normalized, true
					} else if needsToken && (len(args) < 2 || args[1] != workerGitHubOrigin) {
						return refused("worker network git names origin or an explicit GitHub repository URL")
					}
				}
				if needsToken && !request.Write {
					for _, arg := range args[1:] {
						if strings.Contains(arg, "upload-pack") || strings.Contains(arg, "receive-pack") || arg == "-c" || arg == workerGitHubConfigOption || strings.HasPrefix(arg, "--config=") {
							return refused("worker network git cannot execute a custom transport command")
						}
					}
				}
				if verb == "submodule" || verb == "remote-http" || verb == "remote-https" || verb == "send-pack" || verb == "http-fetch" || verb == "http-push" {
					return refused("worker GitHub transport requires origin fetch or its assigned branch push")
				}
				if needsToken {
					// Remote URL is pinned for every network operation, not taken from a
					// worker-editable .git/config. No helper or token is persisted there.
					args = append([]string{"-c", "remote.origin.url=https://github.com/" + request.Repository + ".git", "-c", "remote.origin.pushurl=https://github.com/" + request.Repository + ".git", "-c", workerGitHubNoCredentialHelper, "-c", workerGitHubCredentialHelper, "-c", "http.followRedirects=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
				}

			}
			if binary == "" {
				return refused("host GitHub CLI is not provisioned")
			}
			if !needsToken {
				cwd, cwdErr := os.Getwd()
				if cwdErr != nil {
					return cwdErr
				}
				local := exec.CommandContext(ctx, binary, args...) //nolint:gosec // system git, no credential or configuration injection
				var localEnv []string
				for _, kv := range env {
					if !strings.HasPrefix(kv, "GH_TOKEN=") && !strings.HasPrefix(kv, "GITHUB_TOKEN=") {
						localEnv = append(localEnv, kv)
					}
				}
				local.Dir, local.Env, local.Stdin, local.Stdout, local.Stderr = cwd, localEnv, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
				return workerGitHubCommandError(local.Run())
			}
			var token workerGitHubToken
			if needsToken {
				if err := execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), workerGitHubIssue, j.Handle.Ref, request, j.Request.ControlKey, &token, s.Callback); err != nil {
					return err
				}
				if token.Token == "" || time.Until(token.Expires) < time.Minute {
					return errors.New("coordinator returned no usable worker GitHub credential")
				}
			}
			clean := env[:0]
			for _, kv := range env {
				key, _, _ := strings.Cut(kv, "=")
				if key != "GITHUB_TOKEN" && !strings.HasPrefix(key, "GH_") && !strings.HasPrefix(key, "GIT_") {
					clean = append(clean, kv)
				}
			}
			env = append(clean, "GH_REPO="+request.Repository, "GH_HOST=github.com", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
			if needsToken {
				env = append(env, "GH_TOKEN="+token.Token)
			}
			if name == "github-git" && request.Write {
				isolatedArgs, isolatedEnv, cleanup, err := workerGitHubIsolatedPush(ctx, binary, workDir, request.Repository, targetBranch, env)
				if err != nil {
					return err
				}
				defer cleanup()
				args, env = isolatedArgs, isolatedEnv
			}
			child := exec.CommandContext(ctx, binary, args...) //nolint:gosec // policy-pinned gh or system git; arguments validated above
			child.Dir, child.Env, child.Stdin, child.Stdout, child.Stderr = workDir, env, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
			return workerGitHubCommandError(child.Run())
		}})
	}
	return commands
}

type workerGitHubPushTarget struct {
	Branch string `json:"branch"`
}

func executionGitHubTargetBranch(j execution.Journal) string {
	return execution.GitHubBranch(j.Handle.Ref)
}

func workerGitHubWorkDir(defaultWorkspace, defaultRepo string) (string, string) {
	workDir, repo := defaultWorkspace, strings.ToLower(defaultRepo)
	if defaultWorkspace == "" {
		return workDir, repo
	}
	cleanWorkspace := filepath.Clean(defaultWorkspace)
	execRoot := filepath.Dir(cleanWorkspace)
	cwd, err := os.Getwd()
	if err != nil {
		return workDir, repo
	}
	cwd = filepath.Clean(cwd)
	if cwd != cleanWorkspace && !strings.HasPrefix(cwd, cleanWorkspace+string(filepath.Separator)) && !strings.HasPrefix(cwd, execRoot+string(filepath.Separator)) {
		return workDir, repo
	}
	workDir = cwd
	for dir := cwd; dir != cleanWorkspace && dir != execRoot && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if r := gitConfigOriginRepo(filepath.Join(dir, ".git", "config")); r != "" {
			return workDir, r
		}
	}
	return workDir, repo
}

func gitConfigOriginRepo(configPath string) string {
	b, err := os.ReadFile(filepath.Clean(configPath)) //nolint:gosec // bounded to directories under the execution workspace root
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if inOrigin {
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
				if repo := execution.RepositoryIdentity(strings.TrimSpace(v)); repo != "" {
					return repo
				}
			}
		}
	}
	return ""
}

// Preserve client exit codes (for example git's 128 or gh's pending-check 8)
// rather than collapsing every unsuccessful client command to exit 1.
func workerGitHubCommandError(err error) error {
	if err == nil {
		return nil
	}
	var status *exec.ExitError
	if errors.As(err, &status) {
		code := status.ExitCode()
		if code < 1 {
			code = ExitError
		}
		return &exitError{code: code, msg: err.Error()}
	}
	return fmt.Errorf("worker GitHub command failed: %w", err)
}

// Push from a disposable repository with no workspace refs or configuration.
// Only object storage and the resolved HEAD cross this boundary. In particular,
// multi-valued pushurls, URL rewrites, mirrors, tags, includes, proxy settings,
// custom receive-pack commands and hooks cannot change authenticated dispatch.
func workerGitHubIsolatedPush(ctx context.Context, binary, workspace, repository, branch string, env []string) ([]string, []string, func(), error) {
	env = append(env, "GIT_NO_REPLACE_OBJECTS=1")
	inspect := func(args ...string) (string, error) {
		command := exec.CommandContext(ctx, binary, args...) //nolint:gosec // system git; fixed read-only revision queries without credentials
		command.Dir = workspace
		for _, kv := range env {
			if !strings.HasPrefix(kv, "GH_TOKEN=") {
				command.Env = append(command.Env, kv)
			}
		}
		out, err := command.Output()
		if err != nil {
			return "", errors.New("cannot resolve worker push source")
		}
		return strings.TrimSpace(string(out)), nil
	}
	head, err := inspect("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, nil, nil, err
	}
	common, err := inspect("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, nil, nil, err
	}
	format, err := inspect("rev-parse", "--show-object-format")
	if err != nil {
		return nil, nil, nil, err
	}
	if format != "sha1" && format != "sha256" {
		return nil, nil, nil, errors.New("unsupported worker object format")
	}
	dir, err := os.MkdirTemp("", "beekeeper-push-*")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) } //nolint:gosec // disposable directory returned by MkdirTemp, never a workspace path
	config := "[core]\nrepositoryformatversion = 0\nbare = true\n"
	if format == "sha256" {
		config = "[core]\nrepositoryformatversion = 1\nbare = true\n[extensions]\nobjectFormat = sha256\n"
	}
	for _, name := range []string{"objects", "refs"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			cleanup()
			return nil, nil, nil, err
		}
	}
	for name, contents := range map[string]string{"config": config, workerGitHubHead: head + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil { //nolint:gosec // fixed config and HEAD names in a new private temporary repository
			cleanup()
			return nil, nil, nil, err
		}
	}
	env = append(env, "GIT_DIR="+dir, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+strconv.Quote(filepath.Join(common, "objects")))
	args := []string{"-c", workerGitHubNoCredentialHelper, "-c", workerGitHubCredentialHelper, "-c", "http.followRedirects=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "push.followTags=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "push", "--no-follow-tags", "https://github.com/" + repository + ".git", head + ":refs/heads/" + branch}
	return args, env, cleanup, nil
}
