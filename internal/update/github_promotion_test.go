package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestGitHubKeepsCandidateWhenPromotionIsUnpublished(t *testing.T) {
	const (
		current = "v1.0.0-rc.1"
		next    = "v1.0.0-rc.2"
		stable  = "v1.0.0"
		oldSHA  = "1111111111111111111111111111111111111111"
		newSHA  = "2222222222222222222222222222222222222222"
	)
	for _, state := range []string{"absent", "draft", "published"} {
		for _, running := range []string{current, "v0.9.0"} {
			t.Run(state+"/"+running, func(t *testing.T) {
				var requested []string
				stubGitHub(t, running, func(r *http.Request) (int, string, http.Header) {
					if strings.Contains(r.URL.Path, matchingRefsPath) {
						return http.StatusOK, refsWithSHAJSON([][2]string{
							{current, oldSHA}, {next, newSHA}, {stable, newSHA}, {"v0.9.0", oldSHA},
						}), nil
					}
					if strings.Contains(r.URL.Path, "/compare/") {
						return http.StatusOK, aheadCompare, nil
					}
					tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
					requested = append(requested, tag)
					if tag == stable {
						switch state {
						case "absent":
							return http.StatusNotFound, `{}`, nil
						case "draft":
							return http.StatusOK, strings.TrimSuffix(releaseJSON(tag, true, true), "}") + `,"draft":true}`, nil
						}
					}
					return http.StatusOK, releaseJSON(tag, true, true), nil
				})
				res, err := Run(context.Background(), io.Discard, true)
				wantLatest, wantErr := stable, ErrOutdated
				wantRequested := []string{stable}
				if state != "published" {
					if running == current {
						wantLatest = next
						wantRequested = append(wantRequested, next)
					} else {
						wantLatest, wantErr = running, nil
						wantRequested = append(wantRequested, running)
					}
				}
				if !errors.Is(err, wantErr) || res.Latest != wantLatest || !slices.Equal(requested, wantRequested) {
					t.Fatalf("result=%+v err=%v requested=%v; want Latest=%s err=%v requested=%v", res, err, requested, wantLatest, wantErr, wantRequested)
				}
			})
		}
	}
}

func TestGitHubPrefersStableAcrossBasesAtTheSameCommit(t *testing.T) {
	const stable, next = "v1.0.0", "v1.1.0-rc.1"
	var requested []string
	stubGitHub(t, "v0.9.0-rc.1", func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, matchingRefsPath) {
			return http.StatusOK, refsWithSHAJSON([][2]string{{stable, "1111111"}, {next, "1111111"}}), nil
		}
		tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
		requested = append(requested, tag)
		return http.StatusOK, releaseJSON(tag, true, true), nil
	})
	res, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, ErrOutdated) || res.Latest != stable || !slices.Equal(requested, []string{stable}) {
		t.Fatalf("result=%+v err=%v requested=%v", res, err, requested)
	}
}

func TestGitHubUnpublishedPromotionFallbacksStayBounded(t *testing.T) {
	var refs [][2]string
	for i := 0; i < maxReleaseCandidates+1; i++ {
		sha := fmt.Sprintf("%040d", i+1)
		refs = append(refs, [2]string{fmt.Sprintf("v1.%d.0", i), sha}, [2]string{fmt.Sprintf("v1.%d.0-rc.1", i), sha})
	}
	releases, candidates := 0, 0
	stubGitHub(t, "v0.9.0-rc.1", func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, matchingRefsPath) {
			return http.StatusOK, refsWithSHAJSON(refs), nil
		}
		if strings.Contains(r.URL.Path, "/compare/") {
			return http.StatusOK, aheadCompare, nil
		}
		releases++
		if strings.Contains(r.URL.Path, "-rc.") {
			candidates++
		}
		return http.StatusNotFound, `{}`, nil
	})
	res, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, errIncompleteLookup) || res.Latest != "" || releases != maxReleaseCandidates || candidates == 0 {
		t.Fatalf("result=%+v err=%v releases=%d candidates=%d", res, err, releases, candidates)
	}
}
