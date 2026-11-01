package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const (
	githubStable     = "v0.170.0"
	githubRunningRC  = "v0.171.0-rc.98"
	githubOlderRC    = "v0.171.0-rc.99"
	githubNewerRC    = "v0.171.0-rc.100"
	matchingRefsPath = "/git/matching-refs/"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// GitHub caps anonymous release lists at 1,000 entries, even while its Link
// header points at page 11. Matching refs do not have that release-list cap.
func TestGitHubLookupBeyondAnonymousReleaseLimit(t *testing.T) {
	refs := make([]string, 0, 1201)
	for i := 0; i <= 1200; i++ {
		refs = append(refs, fmt.Sprintf("v0.171.0-rc.%d", i))
	}
	calls := stubGitHub(t, githubRunningRC, func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, matchingRefsPath) {
			return 200, refsJSON(refs...), nil
		}
		if strings.Contains(r.URL.Path, "/releases/tags/") {
			return 200, releaseJSON("v0.171.0-rc.1200", true, true), nil
		}
		// Model the actual API's misleading pagination and terminal 422.
		if r.URL.Query().Get("page") == "11" {
			return 422, `{"message":"Only the first 1000 results are available."}`, nil
		}
		page := 1
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		return 200, "[" + releaseJSON(githubOlderRC, true, true) + "]", http.Header{"Link": []string{fmt.Sprintf(`<https://api.github.com/repos/giantswarm/beekeeper/releases?page=%d>; rel="next"`, page+1)}}
	})
	res, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, ErrOutdated) || res.Latest != "v0.171.0-rc.1200" || *calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d; want rc.1200 in two requests", res, err, *calls)
	}
}

func stubGitHub(t *testing.T, current string, respond func(*http.Request) (int, string, http.Header)) *int {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	oldTransport, oldVersion := http.DefaultTransport, currentVersion
	t.Cleanup(func() { http.DefaultTransport, currentVersion = oldTransport, oldVersion })
	currentVersion = func() string { return current }
	calls := 0
	http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "" {
			t.Error("anonymous lookup sent authorization")
		}
		status, body, header := respond(r)
		if header == nil {
			header = make(http.Header)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	return &calls
}

func refsJSON(tags ...string) string {
	refs := make([]map[string]string, 0, len(tags))
	for _, tag := range tags {
		refs = append(refs, map[string]string{"ref": "refs/tags/" + tag})
	}
	data, _ := json.Marshal(refs)
	return string(data)
}

func releaseJSON(tag string, binary, bundle bool) string {
	assets := make([]map[string]any, 0, 2)
	if binary {
		assets = append(assets, map[string]any{"id": 1, "name": binaryName()})
	}
	if bundle {
		assets = append(assets, map[string]any{"id": 2, "name": binaryName() + ".bundle"})
	}
	data, _ := json.Marshal(map[string]any{"id": 1, "tag_name": tag, "prerelease": strings.Contains(tag, "-"), "assets": assets})
	return string(data)
}

func TestGitHubEmptyRefsRetryCompleteLookup(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			refCalls := 0
			calls := stubGitHub(t, githubRunningRC, func(r *http.Request) (int, string, http.Header) {
				if strings.Contains(r.URL.Path, matchingRefsPath) {
					refCalls++
					if empty || refCalls == 1 {
						return 200, `[]`, nil
					}
					return 200, refsJSON(githubOlderRC, githubNewerRC), nil
				}
				if strings.HasSuffix(r.URL.Path, "/releases/tags/v0.171.0-rc.100") {
					return 200, releaseJSON(githubNewerRC, true, true), nil
				}
				t.Errorf("unexpected fallback request %s", r.URL.Path)
				return 500, `{}`, nil
			})
			res, err := Run(context.Background(), io.Discard, true)
			if empty {
				if err == nil || res.Latest != "" || *calls != 3 {
					t.Fatalf("result=%+v err=%v calls=%d", res, err, *calls)
				}
			} else if !errors.Is(err, ErrOutdated) || res.Latest != githubNewerRC || *calls != 3 {
				t.Fatalf("result=%+v err=%v calls=%d", res, err, *calls)
			}
		})
	}
}

func TestGitHubCandidateEligibility(t *testing.T) {
	for _, current := range []string{githubRunningRC, githubStable} {
		t.Run(current, func(t *testing.T) {
			var requested []string
			stubGitHub(t, current, func(r *http.Request) (int, string, http.Header) {
				if strings.Contains(r.URL.Path, matchingRefsPath) {
					return 200, refsJSON(githubOlderRC, githubNewerRC, "v0.171.0-rc.101", "v0.171.0-rc.102", "v0.171.0-rc.103", "v0.171.0-rc.104", githubStable, "vnot-semver"), nil
				}
				tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
				requested = append(requested, tag)
				switch tag {
				case "v0.171.0-rc.104":
					return 404, `{}`, nil
				case "v0.171.0-rc.103":
					return 200, releaseJSON(tag, true, false), nil
				case "v0.171.0-rc.102":
					return 200, releaseJSON(tag, false, true), nil
				case "v0.171.0-rc.101":
					return 200, strings.TrimSuffix(releaseJSON(tag, true, true), "}") + `,"draft":true}`, nil
				default:
					return 200, releaseJSON(tag, true, true), nil
				}
			})
			res, err := Run(context.Background(), io.Discard, true)
			if strings.Contains(current, "-rc.") {
				if !errors.Is(err, ErrOutdated) || res.Latest != githubNewerRC || len(requested) != 5 {
					t.Fatalf("result=%+v err=%v requested=%v", res, err, requested)
				}
			} else if err != nil || res.Latest != githubStable || len(requested) != 1 {
				t.Fatalf("result=%+v err=%v requested=%v", res, err, requested)
			}
		})
	}
}

func TestGitHubCandidateFailureDoesNotReportOlderRelease(t *testing.T) {
	calls := stubGitHub(t, githubRunningRC, func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, matchingRefsPath) {
			return 200, refsJSON(githubOlderRC, githubNewerRC), nil
		}
		return 500, `{}`, nil
	})
	res, err := Run(context.Background(), io.Discard, true)
	if err == nil || res.Latest != "" || *calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", res, err, *calls)
	}
}

func TestGitHubCandidateRequestsAreBounded(t *testing.T) {
	var tags []string
	for i := 0; i < 100; i++ {
		tags = append(tags, fmt.Sprintf("v0.171.0-rc.%d", i))
	}
	calls := stubGitHub(t, "v0.171.0-rc.0", func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, matchingRefsPath) {
			return 200, refsJSON(tags...), nil
		}
		return 404, `{}`, nil
	})
	res, err := Run(context.Background(), io.Discard, true)
	if err == nil || res.Latest != "" || *calls != 1+maxReleaseCandidates {
		t.Fatalf("result=%+v err=%v calls=%d", res, err, *calls)
	}
}

func TestGitHubLookupCancellation(t *testing.T) {
	calls := stubGitHub(t, running, func(*http.Request) (int, string, http.Header) {
		t.Error("cancelled lookup reached HTTP")
		return 500, `{}`, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, io.Discard, true)
	if !errors.Is(err, context.Canceled) || *calls != 0 {
		t.Fatalf("err=%v calls=%d", err, *calls)
	}
}
