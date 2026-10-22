package update

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/go-selfupdate"
)

// latestIs answers rel as the latest release, or err.
func latestIs(rel selfupdate.SourceRelease, err error) latestFunc {
	return func(_ context.Context, owner, repo string) (selfupdate.SourceRelease, error) {
		if owner+"/"+repo != Repository {
			return nil, errors.New("asked about " + owner + "/" + repo)
		}
		return rel, err
	}
}

// GitHub's release list intermittently answers 200 with no releases.
func TestFindsTheLatestReleaseWhenGitHubListsNone(t *testing.T) {
	setup(t, running, withLatest{Source: &fakeSource{}, latest: latestIs(release(latestV), nil)})
	var out bytes.Buffer
	res, err := Run(context.Background(), &out, true)
	if !errors.Is(err, ErrOutdated) || res.Latest != latestV {
		t.Fatalf("Run(check) = %+v, %v; want %s reported", res, err, latestV)
	}
}

func TestInstallsTheLatestReleaseWhenGitHubListsNone(t *testing.T) {
	setup(t, running, withLatest{
		Source: &fakeSource{assets: map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte("its bundle")}},
		latest: latestIs(release(latestV), nil),
	})
	exe, _ := installed(t)
	newValidator = func() selfupdate.Validator { return &acceptingValidator{} }
	if res, err := Run(context.Background(), io.Discard, false); err != nil || !res.Updated {
		t.Fatalf("Run() = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Clean(exe)); string(got) != newBinary {
		t.Errorf("executable holds %q after the update", got)
	}
}

// The latest release's assets upload after its tag: the newest listed
// release that carries them is the one.
func TestFallsBackToTheNewestReleaseWithItsAssets(t *testing.T) {
	uploading := fakeRelease{tag: "v0.4.0"}
	setup(t, running, withLatest{
		Source: &fakeSource{releases: []selfupdate.SourceRelease{uploading, release(latestV), release(running)}},
		latest: latestIs(uploading, nil),
	})
	res, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, ErrOutdated) || res.Latest != latestV {
		t.Fatalf("Run(check) = %+v, %v; want %s reported", res, err, latestV)
	}
}

func TestLatestLookup(t *testing.T) {
	listed := []selfupdate.SourceRelease{release(latestV)}
	for _, tc := range []struct {
		name    string
		list    []selfupdate.SourceRelease
		latest  latestFunc
		want    []string
		wantErr string
	}{
		{"both", listed, latestIs(release("v0.4.0"), nil), []string{latestV, "v0.4.0"}, ""},
		{"no release yet", nil, latestIs(nil, nil), nil, ""},
		{"latest fails, the list stands", listed, latestIs(nil, errors.New("502")), []string{latestV}, ""},
		{"latest fails, nothing listed", nil, latestIs(nil, errors.New("502")), nil, "502"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := withLatest{Source: &fakeSource{releases: tc.list}, latest: tc.latest}
			rels, err := src.ListReleases(context.Background(), selfupdate.ParseSlug(Repository))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ListReleases() = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var tags []string
			for _, r := range rels {
				tags = append(tags, r.GetTagName())
			}
			if strings.Join(tags, ",") != strings.Join(tc.want, ",") {
				t.Errorf("listed %v, want %v", tags, tc.want)
			}
		})
	}
}

func TestListErrorsStayErrors(t *testing.T) {
	src := withLatest{Source: &fakeSource{err: errors.New("dial tcp: no route to host")}, latest: latestIs(release(latestV), nil)}
	if _, err := src.ListReleases(context.Background(), selfupdate.ParseSlug(Repository)); err == nil {
		t.Error("a failed list was answered")
	}
}
