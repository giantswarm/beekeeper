package github

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// PublishedCandidate is the newest release candidate of repo published at
// or after since whose asset is uploaded: a pre-release, no draft, that
// carries the binary an install would fetch. "" when there is none.
func PublishedCandidate(ctx context.Context, run GH, repo, asset string, since time.Time) (string, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/releases?per_page=10", repo))
	if err != nil {
		return "", err
	}
	var releases []struct {
		Tag         string    `json:"tag_name"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(out, &releases); err != nil {
		return "", fmt.Errorf("the releases of %s: %w", repo, err)
	}
	newest, at := "", time.Time{}
	for _, r := range releases {
		if r.Draft || !r.Prerelease || r.PublishedAt.Before(since) || !r.PublishedAt.After(at) {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == asset && a.State == "uploaded" {
				newest, at = r.Tag, r.PublishedAt
			}
		}
	}
	return newest, nil
}
