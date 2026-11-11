package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/go-selfupdate"
	"github.com/google/go-github/v93/github"

	deskgithub "github.com/giantswarm/beekeeper/internal/github"
)

// Release is one release of Repository as an authenticated lookup found it:
// what an execution host needs to install it without asking GitHub's API
// itself. Its assets are downloaded from their public download URLs, which
// GitHub's API rate limits do not cover, and verified against their bundles
// on the host as any download is.
type Release struct {
	Tag        string            `json:"tag"`
	ID         int64             `json:"id"`
	URL        string            `json:"url"`
	Published  time.Time         `json:"published"`
	Prerelease bool              `json:"prerelease"`
	Assets     []Asset           `json:"assets"`
	Tools      map[string]string `json:"tools,omitempty"`
}

// Asset is one binary or bundle of a Release.
type Asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int    `json:"size"`
	// URL is the asset's public download URL.
	URL string `json:"url"`
}

// LookupVersion looks up release version (a tag, "v0.3.0") of Repository
// with the gh login (or GH_TOKEN, GITHUB_TOKEN), never anonymously, so that
// the call counts in the person's budget like every other GitHub call of
// beekeeper. A coordinator hands the answer to the execution host it
// updates (InstallRelease).
func LookupVersion(ctx context.Context, version string) (Release, error) {
	if _, err := releaseSemver(version); err != nil {
		return Release{}, err
	}
	token, err := authToken()
	if err != nil {
		return Release{}, err
	}
	client, err := github.NewClient(github.WithAuthToken(token))
	if err != nil {
		return Release{}, err
	}
	owner, repo, _ := strings.Cut(Repository, "/")
	lookup, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	if err := deskgithub.AdmitRESTCall(lookup, "update.LookupVersion"); err != nil {
		return Release{}, err
	}
	rel, response, err := client.Repositories.GetReleaseByTag(lookup, owner, repo, version)
	switch {
	case response != nil && response.StatusCode == http.StatusNotFound:
		return Release{}, fmt.Errorf("%s has no release %s", Repository, version)
	case err != nil:
		if limit := rateLimit(err); limit != "" {
			return Release{}, fmt.Errorf("looking up release %s of %s: %s: %w", version, Repository, limit, err)
		}
		return Release{}, fmt.Errorf("looking up release %s of %s: %w", version, Repository, err)
	case rel.GetDraft() || rel.GetTagName() != version:
		return Release{}, fmt.Errorf("%s has no published release %s", Repository, version)
	}
	out := Release{Tag: rel.GetTagName(), ID: rel.GetID(), URL: rel.GetHTMLURL(), Published: rel.GetPublishedAt().Time, Prerelease: rel.GetPrerelease()}
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.GetName(), "beekeeper-") {
			out.Assets = append(out.Assets, Asset{ID: a.GetID(), Name: a.GetName(), Size: a.GetSize(), URL: a.GetBrowserDownloadURL()})
		}
	}
	return out, nil
}

// InstallRelease installs rel, as a coordinator's LookupVersion found it,
// over the running executable once its bundle verifies, as InstallVersion
// does, without asking GitHub's API: the binary and its bundle come from
// their public download URLs under the release's tag.
func InstallRelease(ctx context.Context, w io.Writer, rel Release) (Result, error) {
	return installVersion(ctx, w, rel.Tag, func() (selfupdate.Source, error) { return givenSource{rel: rel}, nil })
}

// givenSource lists exactly the release a coordinator looked up and
// downloads its assets from their download URLs.
type givenSource struct{ rel Release }

func (s givenSource) ListReleases(context.Context, selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	published := github.Timestamp{Time: s.rel.Published}
	r := &github.RepositoryRelease{ID: s.rel.ID, TagName: s.rel.Tag, HTMLURL: s.rel.URL, PublishedAt: &published, Prerelease: s.rel.Prerelease}
	for i := range s.rel.Assets {
		a := &s.rel.Assets[i]
		r.Assets = append(r.Assets, &github.ReleaseAsset{ID: &a.ID, Name: &a.Name, Size: &a.Size, BrowserDownloadURL: &a.URL})
	}
	return []selfupdate.SourceRelease{selfupdate.NewGitHubRelease(r)}, nil
}

func (s givenSource) DownloadReleaseAsset(ctx context.Context, _ *selfupdate.Release, assetID int64) (io.ReadCloser, error) {
	// Only the release's own downloads: the URL comes from the coordinator,
	// and the host fetches nothing else with it.
	prefix := "https://github.com/" + Repository + "/releases/download/" + s.rel.Tag + "/"
	for _, a := range s.rel.Assets {
		if a.ID != assetID {
			continue
		}
		if a.URL != prefix+a.Name {
			return nil, fmt.Errorf("asset %s: download URL %q is not %s's", a.Name, a.URL, prefix)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, http.NoBody)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("downloading %s: %w", a.Name, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("downloading %s: HTTP %d", a.Name, resp.StatusCode)
		}
		return resp.Body, nil
	}
	return nil, fmt.Errorf("asset %d: %w", assetID, selfupdate.ErrAssetNotFound)
}

// rateLimit names a GitHub rate-limit refusal in err with when it ends, ""
// for any other error.
func rateLimit(err error) string {
	var limited *github.RateLimitError
	if errors.As(err, &limited) {
		return "GitHub rate limit exceeded; resets at " + limited.Rate.Reset.UTC().Format(time.RFC3339)
	}
	var secondary *github.AbuseRateLimitError
	if errors.As(err, &secondary) {
		if secondary.RetryAfter != nil {
			return "GitHub secondary rate limit; retry after " + secondary.RetryAfter.String()
		}
		return "GitHub secondary rate limit"
	}
	return ""
}

// errNoToken is a lookup without any GitHub token: beekeeper never asks
// GitHub anonymously.
var errNoToken = errors.New("no GitHub token: log in with `gh auth login` or set GH_TOKEN or GITHUB_TOKEN; beekeeper does not ask GitHub anonymously, whose limit the machine's IP shares")
