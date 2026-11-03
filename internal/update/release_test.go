package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/go-selfupdate"
	"github.com/google/go-github/v93/github"
)

// downloadURL is the public download URL of asset name of release tag.
func downloadURL(tag, name string) string {
	return "https://github.com/" + Repository + "/releases/download/" + tag + "/" + name
}

// apiAsset is asset name of latestV as GitHub's API answers it.
func apiAsset(id int64, name string) *github.ReleaseAsset {
	size, url := 3, downloadURL(latestV, name)
	return &github.ReleaseAsset{ID: &id, Name: &name, Size: &size, BrowserDownloadURL: &url}
}

func TestLookupVersionAsksWithTheLoginAndKeepsTheBinaries(t *testing.T) {
	published := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	calls := stubGitHub(t, running, func(r *http.Request) (int, string, http.Header) {
		if r.URL.Path != "/repos/"+Repository+"/releases/tags/"+latestV {
			return 404, `{"message":"Not Found"}`, nil
		}
		data, _ := json.Marshal(map[string]any{"id": 7, "tag_name": latestV, "html_url": "https://github.com/" + Repository + "/releases/tag/" + latestV, "published_at": published, "assets": []*github.ReleaseAsset{
			apiAsset(1, binaryName()), apiAsset(2, binaryName()+".bundle"), apiAsset(3, "checksums.txt"),
		}})
		return 200, string(data), nil
	})
	rel, err := LookupVersion(context.Background(), latestV)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("%d requests, want one", *calls)
	}
	if rel.Tag != latestV || rel.ID != 7 || !rel.Published.Equal(published) || len(rel.Assets) != 2 || rel.Assets[0].URL != downloadURL(latestV, binaryName()) {
		t.Errorf("looked up %+v", rel)
	}
	if _, err := LookupVersion(context.Background(), "v9.9.9"); err == nil || !strings.Contains(err.Error(), "no release v9.9.9") {
		t.Errorf("a missing release: %v", err)
	}
}

func TestLookupVersionNeverAsksAnonymously(t *testing.T) {
	calls := stubGitHub(t, running, func(*http.Request) (int, string, http.Header) { return 200, "{}", nil })
	githubToken = func(context.Context) (string, error) { return "", errors.New("gh unavailable") }
	if _, err := LookupVersion(context.Background(), latestV); !errors.Is(err, errNoToken) || *calls != 0 {
		t.Fatalf("without a token: %v after %d requests, want a refusal before any", err, *calls)
	}
}

// A refused lookup names the limit and when it lifts, whether the
// coordinator's (LookupVersion) or a host's own (InstallVersion).
func TestReleaseLookupsNameTheRateLimit(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	for name, lookup := range map[string]func() error{
		"lookup": func() error { _, err := LookupVersion(context.Background(), latestV); return err },
		"install": func() error {
			_, err := InstallVersion(context.Background(), io.Discard, latestV)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			stubGitHub(t, running, func(*http.Request) (int, string, http.Header) {
				return 403, `{"message":"API rate limit exceeded"}`, http.Header{
					"X-Ratelimit-Limit": {"5000"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())},
				}
			})
			if err := lookup(); err == nil || !strings.Contains(err.Error(), "rate limit exceeded; resets at "+reset.UTC().Format(time.RFC3339)) {
				t.Fatalf("got %v, want the rate limit with its reset", err)
			}
		})
	}
}

func TestInstallReleaseDownloadsTheGivenReleaseWithoutTheAPI(t *testing.T) {
	setup(t, running, nil)
	exe, _ := installed(t)
	v := &acceptingValidator{}
	newValidator = func() selfupdate.Validator { return v }
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 404, ""
		switch {
		case r.URL.Host != "github.com" || r.Header.Get("Authorization") != "":
			t.Errorf("asked %s, want only the release's public downloads", r.URL)
			status = 500
		case r.URL.String() == downloadURL(latestV, binaryName()):
			status, body = 200, newBinary
		case r.URL.String() == downloadURL(latestV, binaryName()+".bundle"):
			status, body = 200, installBundle
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	rel := Release{Tag: latestV, Assets: []Asset{
		{ID: bundleID, Name: binaryName() + ".bundle", Size: len(installBundle), URL: downloadURL(latestV, binaryName()+".bundle")},
		{ID: binaryID, Name: binaryName(), Size: len(newBinary), URL: downloadURL(latestV, binaryName())},
	}}
	var out bytes.Buffer
	res, err := InstallRelease(context.Background(), &out, rel)
	if err != nil {
		t.Fatalf("InstallRelease() = %v\n%s", err, out.String())
	}
	if got, _ := os.ReadFile(filepath.Clean(exe)); !res.Updated || string(got) != newBinary || string(v.bundle) != installBundle {
		t.Errorf("result %+v, executable %q, bundle %q", res, got, v.bundle)
	}
}

func TestGivenSourceDownloadsOnlyTheReleasesOwnAssets(t *testing.T) {
	s := givenSource{rel: Release{Tag: latestV, Assets: []Asset{
		{ID: 1, Name: binaryName(), URL: "https://example.test/" + binaryName()},
		{ID: 2, Name: binaryName() + ".bundle", URL: downloadURL("v0.1.0", binaryName()+".bundle")},
	}}}
	for id := range int64(2) {
		if _, err := s.DownloadReleaseAsset(context.Background(), nil, id+1); err == nil || !strings.Contains(err.Error(), "is not") {
			t.Errorf("asset %d: %v, want the URL refused", id+1, err)
		}
	}
	if _, err := s.DownloadReleaseAsset(context.Background(), nil, 3); !errors.Is(err, selfupdate.ErrAssetNotFound) {
		t.Errorf("an unknown asset: %v", err)
	}
}
