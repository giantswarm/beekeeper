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
	"time"
)

const (
	githubStable     = "v0.170.0"
	githubRunningRC  = "v0.171.0-rc.98"
	githubOlderRC    = "v0.171.0-rc.99"
	githubNewerRC    = "v0.171.0-rc.100"
	matchingRefsPath = "/git/matching-refs/"
	// behindCompare is a compare whose head is an ancestor of its base.
	behindCompare = `{"status":"behind"}`
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

// stubLogin is the gh login's token in the GitHub stubs.
const stubLogin = "test-login"

// stubGitHub answers GitHub's API with respond, the gh login's token
// required on every request, and counts the requests.
func stubGitHub(t *testing.T, current string, respond func(*http.Request) (int, string, http.Header)) *int {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	oldToken := githubToken
	githubToken = func(context.Context) (string, error) { return stubLogin, nil }
	t.Cleanup(func() { githubToken = oldToken })
	oldTransport, oldVersion, oldCommit := http.DefaultTransport, currentVersion, currentCommit
	t.Cleanup(func() { http.DefaultTransport, currentVersion, currentCommit = oldTransport, oldVersion, oldCommit })
	currentVersion = func() string { return current }
	currentCommit = func() string { return "" }
	calls := 0
	http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+stubLogin {
			t.Error("the lookup did not send the gh login's token")
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
	// The refusal names each tag it read and why it passed it over.
	if !strings.Contains(err.Error(), "v0.171.0-rc.99: no release; v0.171.0-rc.98: no release") || strings.Contains(err.Error(), "v0.171.0-rc.91") || strings.Contains(err.Error(), "online?") {
		t.Errorf("the refusal does not name the tags read, or blames the network: %v", err)
	}
}

// A promotion tag alone must not hide its published release candidate.
func TestGitHubUnpublishedPromotionFallback(t *testing.T) {
	const running, candidate, stable = "v1.0.0-rc.1", "v1.0.0-rc.2", "v1.0.0"
	for _, publication := range []string{"absent", "draft", "published"} {
		for _, current := range []string{running, "v0.9.0"} {
			for _, stamped := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stamped=%v", publication, current, stamped), func(t *testing.T) {
					var requested []string
					stubGitHub(t, current, func(r *http.Request) (int, string, http.Header) {
						if strings.Contains(r.URL.Path, matchingRefsPath) {
							return 200, refsWithSHAJSON([][2]string{
								{running, "1111111"}, {candidate, "2222222"},
								{stable, "2222222"}, {"v0.9.0", "0000000"},
							}), nil
						}
						if strings.Contains(r.URL.Path, "/compare/") {
							return 200, `{"status":"ahead"}`, nil
						}
						tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
						requested = append(requested, tag)
						if tag == stable {
							switch publication {
							case "absent":
								return 404, `{}`, nil
							case "draft":
								return 200, strings.TrimSuffix(releaseJSON(tag, true, true), "}") + `,"draft":true}`, nil
							}
						}
						return 200, releaseJSON(tag, true, true), nil
					})
					if stamped {
						sha := "1111111"
						if current != running {
							sha = "0000000"
						}
						currentCommit = func() string { return sha }
					}
					want, wantRequests := stable, stable
					if publication != "published" {
						want = candidate
						if current != running {
							want = current
						}
						wantRequests += "," + want
					}
					res, err := Run(context.Background(), io.Discard, true)
					var wantErr error
					if want != current {
						wantErr = ErrOutdated
					}
					if !errors.Is(err, wantErr) || res.Latest != want || strings.Join(requested, ",") != wantRequests {
						t.Fatalf("result=%+v err=%v requests=%v; want %s requests=%s", res, err, requested, want, wantRequests)
					}
				})
			}
		}
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
			return 200, behindCompare, nil
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
			return 200, behindCompare, nil
		}
		tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
		return 200, releaseJSON(tag, true, true), nil
	})
	res, err := Run(context.Background(), io.Discard, true)
	if err != nil || res.Newer || res.Latest != runningTag {
		t.Fatalf("result=%+v err=%v; want Latest=%s Newer=false", res, err, runningTag)
	}
}

func TestGitHubLookupAuthentication(t *testing.T) {
	const (
		loginToken     = stubLogin
		githubEnvToken = "test-github"
		ghEnvToken     = "test-gh"
	)
	for _, tc := range []struct{ name, gh, github, login, want string }{
		{name: "login", login: loginToken, want: loginToken},
		{name: "gh environment", gh: ghEnvToken, github: githubEnvToken, login: loginToken, want: ghEnvToken},
		{name: "github environment", github: githubEnvToken, login: loginToken, want: githubEnvToken},
		{name: "no token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubGitHub(t, githubRunningRC, func(r *http.Request) (int, string, http.Header) { return 200, "[]", nil })
			t.Setenv("GH_TOKEN", tc.gh)
			t.Setenv("GITHUB_TOKEN", tc.github)
			githubToken = func(context.Context) (string, error) {
				if tc.login == "" {
					return "", errors.New("gh unavailable")
				}
				return tc.login, nil
			}
			requests := 0
			http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				want := ""
				if tc.want != "" {
					want = "Bearer " + tc.want
				}
				if r.Header.Get("Authorization") != want {
					t.Error("unexpected authentication source")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[]")), Request: r}, nil
			})
			_, err := Run(context.Background(), io.Discard, true)
			if tc.want == "" {
				// Never anonymously: the machine's IP shares that limit.
				if !errors.Is(err, errNoToken) || requests != 0 {
					t.Fatalf("without a token: %v after %d requests, want a refusal before any", err, requests)
				}
				return
			}
			if requests != emptyRefAttempts {
				t.Fatalf("got %d requests, want %d", requests, emptyRefAttempts)
			}
		})
	}
}

func TestGitHubLookupRateLimitReset(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	stubGitHub(t, githubRunningRC, func(r *http.Request) (int, string, http.Header) {
		return 403, `{"message":"API rate limit exceeded"}`, http.Header{
			"X-Ratelimit-Limit": {"60"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())},
		}
	})
	_, err := Run(context.Background(), io.Discard, true)
	if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), reset.UTC().Format(time.RFC3339)) || strings.Contains(err.Error(), "online?") {
		t.Fatalf("expected rate limit and reset time, got %v", err)
	}
}

// A running release takes the newest release even while release candidates
// of a later base, not promoted yet, are ahead of every release: on one
// line of history each release is behind those candidates, and none of
// them is stale for it. A running candidate still takes the newest
// candidate.
func TestGitHubReleaseBehindALaterBasesCandidates(t *testing.T) {
	// One line of history, oldest first: each tag on its own commit.
	order := []string{"v0.174.3", "v0.174.4", "v0.174.5", "v0.174.6", "v0.175.0-rc.1", "v0.175.0-rc.2", "v0.175.0-rc.3", "v0.175.0-rc.4", "v0.175.0-rc.5", "v0.175.0-rc.6"}
	tagSHAs := make([][2]string, 0, len(order))
	position := make(map[string]int, len(order))
	for i, tag := range order {
		sha := fmt.Sprintf("%07x", 0xa000000+i)
		tagSHAs = append(tagSHAs, [2]string{tag, sha})
		position[sha] = i
	}
	refs := refsWithSHAJSON(tagSHAs)
	for _, tc := range []struct{ current, want string }{
		{"v0.174.4", "v0.174.6"},
		{"v0.175.0-rc.5", "v0.175.0-rc.6"},
	} {
		t.Run(tc.current, func(t *testing.T) {
			stubGitHub(t, tc.current, func(r *http.Request) (int, string, http.Header) {
				if strings.Contains(r.URL.Path, matchingRefsPath) {
					return 200, refs, nil
				}
				if _, compare, ok := strings.Cut(r.URL.Path, "/compare/"); ok {
					base, head, _ := strings.Cut(compare, "...")
					switch b, h := position[base], position[head]; {
					case h < b:
						return 200, behindCompare, nil
					case h > b:
						return 200, `{"status":"ahead"}`, nil
					}
					return 200, `{"status":"identical"}`, nil
				}
				tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
				return 200, releaseJSON(tag, true, true), nil
			})
			res, err := Run(context.Background(), io.Discard, true)
			if !errors.Is(err, ErrOutdated) || !res.Newer || res.Latest != tc.want {
				t.Fatalf("result=%+v err=%v; want Latest=%s ErrOutdated", res, err, tc.want)
			}
		})
	}
}
