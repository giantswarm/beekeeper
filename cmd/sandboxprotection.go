package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

var protectionRepository = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)

func (a *app) sandboxProtectionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "protection <owner/repo> <branch>",
		Short: "Read branch protection through the host broker",
		Long:  "Read classic branch protection with the host's gh login. The broker permits only GET for repositories in github.allowedOrgs. The host login must have administration read access; credentials stay on the host. A missing protection or insufficient access returns GitHub's error. Rulesets are separate from classic protection.",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.brokeredReply(sandbox.Request{Op: sandbox.OpProtection, Args: args})
		},
	}
}

func protectionArgs(args, owners []string) (string, string, error) {
	if len(args) != 2 || !protectionRepository.MatchString(args[0]) || args[1] == "" || strings.ContainsAny(args[1], "\x00\r\n") {
		return "", "", fmt.Errorf("protection requires owner/repo and a branch only")
	}
	owner, repository, _ := strings.Cut(args[0], "/")
	if repository == "." || repository == ".." || args[1] == "." || args[1] == ".." {
		return "", "", fmt.Errorf("invalid repository or branch")
	}
	for _, allowed := range owners {
		if strings.EqualFold(owner, allowed) {
			return args[0], args[1], nil
		}
	}
	return "", "", fmt.Errorf("protection repository is outside github.allowedOrgs")
}

func (a *app) brokeredProtection() sandbox.Handler {
	return func(ctx context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
		repo, branch, err := protectionArgs(req.Args, a.cfg.GitHub.AllowedOrgs)
		if err != nil {
			return sandbox.Reply{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		token, err := personToken(ctx, a.cfg.Agents.Shell.Path)
		if err != nil {
			return sandbox.Reply{}, err
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		out, err := readProtection(ctx, client, token, repo, branch)
		return sandbox.Reply{Out: out}, err
	}
}

func readProtection(ctx context.Context, client *http.Client, token, repo, branch string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/branches/"+url.PathEscape(branch)+"/protection", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("branch protection read: HTTP %d", resp.StatusCode)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return "", err
	}
	if len(out) > 4<<20 {
		return "", fmt.Errorf("branch protection response exceeds 4 MiB")
	}
	return string(out), nil
}
