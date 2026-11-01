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
	oldTransport, oldVersion, oldCommit := http.DefaultTransport, currentVersion, currentCommit
	t.Cleanup(func() { http.DefaultTransport, currentVersion, currentCommit = oldTransport, oldVersion, oldCommit })
	currentVersion = func() string { return current }
	currentCommit = func() string { return "" }
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

func refsWithSHAJSON(tagSHAs [][2]string) string {
	refs := make([]map[string]any, 0, len(tagSHAs))
	for _, ts := range tagSHAs {
		refs = append(refs, map[string]any{
			"ref":    "refs/tags/" + ts[0],
			"object": map[string]string{"sha": ts[1]},
		})
	}
	data, _ := json.Marshal(refs)
	return string(data)
}

// A stable release cut at an older release candidate's commit (for example
// v0.171.0 at v0.171.0-rc.110 after v0.171.0-rc.120 is out) is skipped so
// neither pre-release nor stable installs downgrade onto it; a stable release
// promoted from the newest candidate updates normally.
func TestGitHubSkipsStableReleaseOlderThanReleaseCandidates(t *testing.T) {
	const (
		staleStable     = "v0.171.0"
		latestRC        = "v0.171.0-rc.120"
		latestRCSHA     = "0dbeb8f"
		staleRCSHA      = "3cccbe5"
		promotedRC1     = "v0.172.0-rc.1"
		promotedRC2     = "v0.172.0-rc.2"
		promotedRCSHA   = "89f004f"
		nextPromoted    = "v0.172.0"
		newestFinal     = "v0.172.2"
		newestFinalSHA  = "0668790"
		aheadCompare    = `{"status":"ahead"}`
		comparePathPart = "/compare/"
	)
	refs := refsWithSHAJSON([][2]string{
		{githubStable, "1111111"},
		{"v0.171.0-rc.110", staleRCSHA},
		{latestRC, latestRCSHA},
		{staleStable, staleRCSHA},
	})
	for _, tc := range []struct {
		current    string
		wantLatest string
		wantNewer  bool
		wantErr    error
	}{
		{latestRC, latestRC, false, nil},
		{"v0.171.0-rc.115", latestRC, true, ErrOutdated},
		{githubStable, githubStable, false, nil},
	} {
		t.Run(tc.current, func(t *testing.T) {
			stubGitHub(t, tc.current, func(r *http.Request) (int, string, http.Header) {
				if strings.Contains(r.URL.Path, matchingRefsPath) {
					return 200, refs, nil
				}
				tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
				if tag == staleStable {
					t.Errorf("requested stale stable release %s", tag)
				}
				return 200, releaseJSON(tag, true, true), nil
			})
			res, err := Run(context.Background(), io.Discard, true)
			if !errors.Is(err, tc.wantErr) || res.Newer != tc.wantNewer || res.Latest != tc.wantLatest {
				t.Fatalf("result=%+v err=%v; want Latest=%s Newer=%v err=%v", res, err, tc.wantLatest, tc.wantNewer, tc.wantErr)
			}
		})
	}

	promotedRefs := refsWithSHAJSON([][2]string{
		{promotedRC1, "8a2daed"},
		{promotedRC2, promotedRCSHA},
		{nextPromoted, promotedRCSHA},
		{"v0.172.1", "63a2f20"},
		{newestFinal, newestFinalSHA},
	})
	for _, tc := range []struct {
		current    string
		wantLatest string
	}{
		{promotedRC1, newestFinal},
		{promotedRC2, newestFinal},
	} {
		t.Run("promoted-"+tc.current, func(t *testing.T) {
			stubGitHub(t, tc.current, func(r *http.Request) (int, string, http.Header) {
				if strings.Contains(r.URL.Path, matchingRefsPath) {
					return 200, promotedRefs, nil
				}
				if strings.Contains(r.URL.Path, comparePathPart) {
					return 200, aheadCompare, nil
				}
				tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
				if tag == promotedRC2 {
					t.Errorf("requested superseded pre-release %s after %s was cut on the same commit", tag, nextPromoted)
				}
				return 200, releaseJSON(tag, true, true), nil
			})
			res, err := Run(context.Background(), io.Discard, true)
			if !errors.Is(err, ErrOutdated) || !res.Newer || res.Latest != tc.wantLatest {
				t.Fatalf("result=%+v err=%v; want Latest=%s ErrOutdated", res, err, tc.wantLatest)
			}
		})
	}

	// A pre-release at an older commit than the newest final release is skipped
	// so a pre-release binary takes the newest of all releases and pre-releases
	// by commit order.
	commitOrderRefs := refsWithSHAJSON([][2]string{
		{promotedRC1, "8a2daed"},
		{promotedRC2, promotedRCSHA},
		{nextPromoted, promotedRCSHA},
		{"v0.172.1", "63a2f20"},
		{newestFinal, newestFinalSHA},
		{"v0.173.0-rc.1", "7777777"},
	})
	stubGitHub(t, promotedRC1, func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, matchingRefsPath) {
			return 200, commitOrderRefs, nil
		}
		if strings.Contains(r.URL.Path, comparePathPart+newestFinalSHA+"...7777777") {
			return 200, `{"status":"behind"}`, nil
		}
		if strings.Contains(r.URL.Path, comparePathPart) {
			return 200, aheadCompare, nil
		}
		tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
		return 200, releaseJSON(tag, true, true), nil
	})
	res, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, ErrOutdated) || !res.Newer || res.Latest != newestFinal {
		t.Fatalf("result=%+v err=%v; want Latest=%s ErrOutdated", res, err, newestFinal)
	}
}

// A release whose commit is an ancestor of the running binary's commit (as
// reported by GitHub's compare endpoint) is skipped so self-update never
// downgrades to an older commit.
func TestGitHubSkipsReleaseBehindRunningCommit(t *testing.T) {
	const (
		runningTag = "v0.171.0-rc.120"
		runningSHA = "0dbeb8f"
	)
	oldCommit := currentCommit
	t.Cleanup(func() { currentCommit = oldCommit })
	currentCommit = func() string { return runningSHA }

	refs := refsWithSHAJSON([][2]string{
		{runningTag, runningSHA},
		{"v0.171.0", "deadbeef"},
	})
	stubGitHub(t, runningTag, func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, matchingRefsPath) {
			return 200, refs, nil
		}
		if strings.Contains(r.URL.Path, "/compare/"+runningSHA+"...deadbeef") {
			return 200, `{"status":"behind"}`, nil
		}
		tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
		return 200, releaseJSON(tag, true, true), nil
	})
	res, err := Run(context.Background(), io.Discard, true)
	if err != nil || res.Newer || res.Latest != runningTag {
		t.Fatalf("result=%+v err=%v; want Latest=%s Newer=false", res, err, runningTag)
	}
}
