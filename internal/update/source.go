package update

import (
	"context"
	"net/http"
	"os"

	"github.com/giantswarm/go-selfupdate"
	"github.com/google/go-github/v93/github"
)

// latestFunc returns a repository's latest release, nil when it has none.
type latestFunc func(ctx context.Context, owner, repo string) (selfupdate.SourceRelease, error)

// withLatest is a release source whose list also carries the repository's
// latest release, asked for on its own. GitHub's release list intermittently
// answers 200 with no releases at all, which go-selfupdate takes for "no
// release carries a binary"; the latest release is answered reliably. The
// list stays: while the latest release's assets are still uploading, the
// newest release that carries them is in it.
type withLatest struct {
	selfupdate.Source
	latest latestFunc
}

// ListReleases returns the listed releases and the latest release. A failed
// latest lookup is an error only when the list is empty too.
func (s withLatest) ListReleases(ctx context.Context, repository selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	rels, err := s.Source.ListReleases(ctx, repository)
	if err != nil {
		return nil, err
	}
	owner, repo, err := repository.GetSlug()
	if err != nil {
		return nil, err
	}
	latest, err := s.latest(ctx, owner, repo)
	switch {
	case err != nil && len(rels) == 0:
		return nil, err
	case err != nil, latest == nil:
		return rels, nil
	}
	return append(rels, latest), nil
}

// newGitHubSource is GitHub's release source, anonymous unless GITHUB_TOKEN
// is set, with the latest release added to its list.
func newGitHubSource() (selfupdate.Source, error) {
	token := os.Getenv("GITHUB_TOKEN")
	src, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token})
	if err != nil {
		return nil, err
	}
	api := github.NewClient(nil)
	if token != "" {
		api = api.WithAuthToken(token)
	}
	return withLatest{Source: src, latest: func(ctx context.Context, owner, repo string) (selfupdate.SourceRelease, error) {
		rel, res, err := api.Repositories.GetLatestRelease(ctx, owner, repo)
		if res != nil && res.StatusCode == http.StatusNotFound {
			// No release yet.
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return selfupdate.NewGitHubRelease(rel), nil
	}}, nil
}
