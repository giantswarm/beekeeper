package update

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/giantswarm/go-selfupdate"
	"github.com/google/go-github/v93/github"
)

// Bound candidate reads when tags have no published, signed binary. A normal
// lookup uses two small requests, independent of the release history's size.
const maxReleaseCandidates = 8
const emptyRefAttempts = 3

type githubSource struct {
	selfupdate.Source // retain the upstream asset downloader
	api               *github.Client
	prereleases       bool
}

func newGitHubSource(prereleases bool) (selfupdate.Source, error) {
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
	if err != nil {
		return nil, err
	}
	var opts []github.ClientOptionsFunc
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	client, err := github.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &githubSource{Source: source, api: client, prereleases: prereleases}, nil
}

func (s *githubSource) ListReleases(ctx context.Context, repository selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	owner, repo, err := repository.GetSlug()
	if err != nil {
		return nil, err
	}
	// Release lists include every historical body and asset and stop at 1,000
	// anonymous results. Matching refs carry just tag metadata without that cap.
	var refs []*github.Reference
	for attempt := 0; attempt < emptyRefAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var response *github.Response
		refs, response, err = s.api.Git.ListMatchingRefs(ctx, owner, repo, "tags/v")
		if err != nil {
			if response != nil && response.StatusCode == http.StatusNotFound {
				return nil, nil
			}
			return nil, err
		}
		if len(refs) != 0 {
			break
		}
	}
	type candidate struct {
		tag     string
		version *semver.Version
	}
	candidates := make([]candidate, 0, len(refs))
	for _, ref := range refs {
		tag := strings.TrimPrefix(ref.GetRef(), "refs/tags/")
		version, err := semver.NewVersion(tag)
		if err != nil || (!s.prereleases && version.Prerelease() != "") {
			continue
		}
		candidates = append(candidates, candidate{tag, version})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].version.Equal(candidates[j].version) {
			return candidates[i].tag < candidates[j].tag
		}
		return candidates[i].version.GreaterThan(candidates[j].version)
	})
	for i, candidate := range candidates {
		if i == maxReleaseCandidates {
			return nil, fmt.Errorf("no signed binary found in the newest %d eligible release tags; refusing an incomplete lookup", maxReleaseCandidates)
		}
		release, response, err := s.api.Repositories.GetReleaseByTag(ctx, owner, repo, candidate.tag)
		if response != nil && response.StatusCode == http.StatusNotFound {
			continue
		} // tag without a release
		if err != nil {
			return nil, err
		}
		if release.GetDraft() || (!s.prereleases && release.GetPrerelease()) {
			continue
		}
		if release.GetTagName() != candidate.tag {
			return nil, fmt.Errorf("release tag %q does not match requested tag %q", release.GetTagName(), candidate.tag)
		}
		name := "beekeeper-" + runtime.GOOS + "-" + runtime.GOARCH
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binary, bundle := false, false
		for _, asset := range release.Assets {
			binary = binary || asset.GetName() == name
			bundle = bundle || asset.GetName() == name+".bundle"
		}
		if binary && bundle {
			return []selfupdate.SourceRelease{selfupdate.NewGitHubRelease(release)}, nil
		}
	}
	return nil, nil
}
