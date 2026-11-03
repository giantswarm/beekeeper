package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// The helper contains no credential; it expands the client-only environment.
//
//nolint:gosec // G101: a credential helper template, not a hardcoded token
const workerGitHubCredentialHelper = `credential.https://github.com.helper=!f() { printf 'username=x-access-token\npassword=%s\n' "$GH_TOKEN"; }; f`

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
			request := workerGitHubRequest{Repository: strings.ToLower(j.Request.Repository)}
			env := os.Environ()
			var binary string
			needsToken := true
			if name == "github-cli" {
				if len(args) == 0 || args[0] == "auth" || args[0] == "alias" || args[0] == "extension" || args[0] == workerGitHubSearch {
					return refused("worker gh requires a repository command")
				}
				request.Repository, err = execution.GitHubRepository(args, request.Repository)
				if err != nil {
					return err
				}
				request.Write = len(args) > 1 && args[0] == "pr" && (args[1] == workerGitHubCreate || args[1] == "edit" || args[1] == "comment")
				// Mutation authority is never inferred from arbitrary API request bodies.
				if len(args) > 1 && args[0] == "pr" && args[1] == gitMerge {
					return refused("merges stay on the coordinator")
				}
				if args[0] == "api" {
					for _, arg := range args[1:] {
						lower := strings.ToLower(arg)
						if strings.Contains(lower, "mutation") || strings.Contains(lower, "/merge") || lower == "post" || lower == "put" || lower == "patch" || lower == "delete" || lower == "--input" {
							return refused("worker gh api supports reads only")
						}
					}
				}
				if request.Write && args[1] == workerGitHubCreate {
					branch := execution.GitHubBranch(j.Handle.Ref)
					for i, arg := range args {
						if arg == "--head" || arg == "-H" {
							if i+1 >= len(args) || args[i+1] != branch {
								return refused("PR must use its execution branch")
							}
						}
						if strings.HasPrefix(arg, "--head=") && arg != "--head="+branch {
							return refused("PR must use its execution branch")
						}
					}
					args = append(args, "--head", branch)
				}
				binary = s.Config.Tools[execution.ToolGitHub].Executable
			} else {
				if err := execution.ValidateWorkerPush(args, execution.GitHubBranch(j.Handle.Ref)); err != nil {
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
					if err := execution.ValidateWorkerPush(args, execution.GitHubBranch(j.Handle.Ref)); err != nil {
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
					args = append([]string{"-c", "remote.origin.url=https://github.com/" + j.Request.Repository + ".git", "-c", "remote.origin.pushurl=https://github.com/" + j.Request.Repository + ".git", "-c", "credential.helper=", "-c", workerGitHubCredentialHelper, "-c", "http.followRedirects=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
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
				return local.Run()
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
				if key != "GITHUB_TOKEN" && !strings.HasPrefix(key, "GH_") && !strings.HasPrefix(key, "GIT_CONFIG_") {
					clean = append(clean, kv)
				}
			}
			env = append(clean, "GH_REPO="+request.Repository, "GH_HOST=github.com", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
			if needsToken {
				env = append(env, "GH_TOKEN="+token.Token)
			}
			child := exec.CommandContext(ctx, binary, args...) //nolint:gosec // policy-pinned gh or system git; arguments validated above
			child.Dir, child.Env, child.Stdin, child.Stdout, child.Stderr = j.Handle.Workspace, env, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := child.Run(); err != nil {
				return fmt.Errorf("worker GitHub command failed: %w", err)
			}
			return nil
		}})
	}
	return commands
}
