package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// githubRenew is how often the broker reads devctl's App token into the
// masked files: devctl renews it once under ten minutes are left, so the
// files never hold a token with less than five.
const githubRenew = 5 * time.Minute

// gateBrokeredTimeout bounds a sandboxed session's gated devctl command on
// the host: the wait for its turn, devctl's own --timeout (45m for a merge)
// and the release wait after it.
const gateBrokeredTimeout = 3 * time.Hour

func sandboxGitCredentialCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "git-credential <get|store|erase>",
		Short: "git's credential helper in the agent sandbox",
		Long: `git-credential is the credential helper the sandbox policy gives git for
https://github.com: it answers a get with the masked Basic credential in
$GH_CONFIG_DIR, which beekeeper sandbox broker keeps current from devctl's
App login, as an authtype=Basic credential (git 2.46 or newer). git sends it
as written, and the sandbox proxy puts the real token in its place on the
way to github.com. store and erase do nothing.`,
		Hidden:            true,
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := os.Getenv("GH_CONFIG_DIR")
			if dir == "" {
				return refused("GH_CONFIG_DIR is not set: the sandbox policy sets it to the broker's masked GitHub login")
			}
			return sandbox.GitCredential(args[0], cmd.InOrStdin(), cmd.OutOrStdout(), dir)
		},
	}
}

// keepGitHub keeps the masked GitHub token current for the sandboxed
// sessions until ctx ends; without a runtime directory there is none.
func (a *app) keepGitHub(ctx context.Context) {
	dir := sandbox.GitHubDir(os.Getenv("XDG_RUNTIME_DIR"))
	if dir == "" {
		_, _ = fmt.Fprintln(a.out, "github token: no XDG_RUNTIME_DIR, sandboxed sessions get no GitHub token")
		return
	}
	devctl := a.cfg.Sandbox.Devctl
	sandbox.KeepGitHub(ctx, dir, githubRenew, func(ctx context.Context) (string, error) {
		return devctlToken(ctx, devctl)
	}, func(line string) { _, _ = fmt.Fprintln(a.out, line) })
}

// devctlToken is devctl's App user token: devctl auth exec hands it to a
// command's environment only, so the command prints it into this process.
func devctlToken(ctx context.Context, devctl string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var out, errOut bytes.Buffer
	c := exec.CommandContext(ctx, devctl, "auth", "exec", "--", "printenv", "GH_TOKEN") //nolint:gosec // the configured devctl
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("devctl auth exec: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// gateBrokered is a gated devctl command in the agent sandbox, where devctl
// finds neither its keychain nor the user's service manager: the host's
// broker runs the same gate as this session and answers its output and
// exit code.
func (a *app) gateBrokered(argv []string, wait time.Duration, queued bool) error {
	if queued {
		return gateRefused("a queued merge runs on the host only")
	}
	return a.brokeredReplyWithin(sandbox.Request{Op: sandbox.OpGate, Args: argv, Wait: wait.String()}, gateBrokeredTimeout+time.Minute)
}

// brokeredGateArgv is the command line of a brokered gate: devctl by its
// configured name, one of the gated commands, the wait bounded.
func brokeredGateArgv(req sandbox.Request, _ bool) ([]string, error) {
	if len(req.Args) == 0 || filepath.Base(req.Args[0]) != merge.Tool {
		return nil, errors.New("the sandbox broker gates devctl only")
	}
	argv := append([]string{merge.Tool}, req.Args[1:]...)
	if _, _, ok := parseGated(argv); !ok && !merge.ParseOwned(argv) {
		return nil, errors.New("the sandbox broker runs devctl pr merge, pr wait, release promote, release wait and rollout wait only")
	}
	wait, err := time.ParseDuration(req.Wait)
	if err != nil || wait <= 0 || wait > gateBrokeredTimeout {
		return nil, fmt.Errorf("wait %q: want a duration up to %s", req.Wait, gateBrokeredTimeout)
	}
	return append([]string{"gate", "--wait", wait.String(), "--"}, argv...), nil
}

// devctlPath is PATH with the configured devctl's directory first, for a
// brokered gate that runs devctl by name.
func devctlPath(devctl string) []string {
	if !filepath.IsAbs(devctl) {
		return nil
	}
	return []string{"PATH=" + filepath.Dir(devctl) + string(os.PathListSeparator) + os.Getenv("PATH")}
}
