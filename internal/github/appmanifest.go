package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// AppManifest is a GitHub App's declaration in the organisation's
// repository: its name and the hosts of its callback URLs, read from the
// manifest on the default branch.
type AppManifest struct {
	// Name is the App's name.
	Name string
	// Callbacks are the hosts (with a port where the URL has one) of the
	// manifest's callback_urls, lower-case, each once.
	Callbacks []string
	// Source is where it was read: <repo>@<commit>:<path>.
	Source string
}

// ReadAppManifest reads the App manifest at path in repo's default branch:
// its name, its callback hosts and the commit it was read at. A manifest
// that is missing, unreadable or names no callback URL is an error: the App
// is not declared.
func ReadAppManifest(ctx context.Context, run GH, repo, path string) (AppManifest, error) {
	out, err := run(ctx, "api", "repos/"+repo+"/commits/HEAD", "--jq", ".sha")
	if err != nil {
		return AppManifest{}, err
	}
	sha := strings.TrimSpace(string(out))
	raw, err := run(ctx, "api", "-H", "Accept: application/vnd.github.raw", fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, path, sha))
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return AppManifest{}, fmt.Errorf("%s declares no App at %s", repo, path)
		}
		return AppManifest{}, err
	}
	var m struct {
		Name         string   `json:"name"`
		CallbackURLs []string `json:"callback_urls"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return AppManifest{}, fmt.Errorf("%s:%s: %w", repo, path, err)
	}
	a := AppManifest{Name: m.Name, Source: fmt.Sprintf("%s@%.12s:%s", repo, sha, path)}
	for _, c := range m.CallbackURLs {
		u, err := url.Parse(c)
		if err != nil || u.Host == "" {
			return AppManifest{}, fmt.Errorf("%s: callback URL %q has no host", a.Source, c)
		}
		if h := strings.ToLower(u.Host); !slices.Contains(a.Callbacks, h) {
			a.Callbacks = append(a.Callbacks, h)
		}
	}
	if a.Name == "" || len(a.Callbacks) == 0 {
		return AppManifest{}, fmt.Errorf("%s declares no App name and callback URL", a.Source)
	}
	return a, nil
}
