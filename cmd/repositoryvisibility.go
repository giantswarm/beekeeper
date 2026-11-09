package cmd

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
)

var repositoryVisibilityGH github.GH = github.RunGH

func (a *app) repositoryVisibilityPolicy(ctx context.Context) github.VisibilityPolicy {
	return github.VisibilityPolicy{PublicOnlyOwners: a.cfg.GitHub.PublicOnlyOwners, Read: func(repo string) (string, error) {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return github.RepositoryVisibility(bounded, repositoryVisibilityGH, repo)
	}}
}

func (a *app) checkRepositoryVisibility(ctx context.Context, repo string) error {
	if err := a.repositoryVisibilityPolicy(ctx).Check(repo); err != nil {
		return refused("%s", err)
	}
	return nil
}

// Check every named repository before any claim, checkout or worker launch.
func (a *app) admitTaskVisibility(ctx context.Context, brief, checkout string) error {
	if len(a.cfg.GitHub.PublicOnlyOwners) == 0 {
		return nil
	}
	repos := map[string]bool{}
	for _, pattern := range []*regexp.Regexp{taskRepository, taskRepositoryReference} {
		for _, match := range pattern.FindAllStringSubmatch(brief, -1) {
			repos[strings.ToLower(match[1])] = true
		}
	}
	if checkout != "" {
		remote, err := runGit(ctx, checkout, "remote", "get-url", "origin")
		if err != nil {
			return refused("cannot resolve checkout repository for visibility policy")
		}
		repo := execution.RepositoryIdentity(strings.TrimSpace(string(remote)))
		if repo == "" {
			return refused("cannot resolve checkout repository for visibility policy")
		}
		repos[repo] = true
	}
	for repo := range repos {
		if err := a.checkRepositoryVisibility(ctx, repo); err != nil {
			return err
		}
	}
	return nil
}
