package update

import (
	"context"
	"net/http"
	"os"

	"github.com/giantswarm/go-selfupdate"
	"github.com/google/go-github/v93/github"
)

// githubSource retains the upstream downloader and empty-list fallback, but
// lists every page: GitHub's release order is not semantic version order.
type githubSource struct {
	selfupdate.Source
	api *github.Client
}

func newGitHubSource() selfupdate.Source {
	source, _ := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
	var opts []github.ClientOptionsFunc
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	client, _ := github.NewClient(opts...)
	return &githubSource{Source: source, api: client}
}

func (s *githubSource) ListReleases(ctx context.Context, repository selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	owner, repo, err := repository.GetSlug()
	if err != nil {
		return nil, err
	}
	var releases []selfupdate.SourceRelease
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, response, err := s.api.Repositories.ListReleases(ctx, owner, repo, opts)
		if err != nil {
			if opts.Page == 0 && response != nil && response.StatusCode == http.StatusNotFound {
				return nil, nil
			}
			return nil, err
		}
		for _, release := range page {
			releases = append(releases, selfupdate.NewGitHubRelease(release))
		}
		if response.NextPage == 0 {
			break
		}
		opts.Page = response.NextPage
	}
	if len(releases) == 0 {
		return s.Source.ListReleases(ctx, repository)
	}
	return releases, nil
}
