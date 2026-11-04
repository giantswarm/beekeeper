package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestGitHubStampedRunningCommitWithoutMatchingTagRef(t *testing.T) {
	const (
		current    = "v0.170.0"
		nextTag    = "v0.171.0"
		runningSHA = "1111111111111111111111111111111111111111"
		staleSHA   = "2222222222222222222222222222222222222222"
		nextSHA    = "3333333333333333333333333333333333333333"
	)
	for _, withOldRef := range []bool{false, true} {
		name := "absent"
		if withOldRef {
			name = "different"
		}
		t.Run(name, func(t *testing.T) {
			refs := [][2]string{{"v0.172.0", staleSHA}, {nextTag, nextSHA}}
			if withOldRef {
				refs = append(refs, [2]string{current, "4444444444444444444444444444444444444444"})
			}
			var compared, requested []string
			stubGitHub(t, current, func(r *http.Request) (int, string, http.Header) {
				if strings.Contains(r.URL.Path, matchingRefsPath) {
					return http.StatusOK, refsWithSHAJSON(refs), nil
				}
				if strings.Contains(r.URL.Path, "/compare/") {
					comparison := strings.SplitN(r.URL.Path, "/compare/", 2)[1]
					compared = append(compared, comparison)
					if comparison == runningSHA+"..."+staleSHA {
						return http.StatusOK, behindCompare, nil
					}
					return http.StatusOK, `{"status":"ahead"}`, nil
				}
				tag := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/beekeeper/releases/tags/")
				requested = append(requested, tag)
				return http.StatusOK, releaseJSON(tag, true, true), nil
			})
			// stubGitHub resets this seam; stamp it after installing the fixture.
			currentCommit = func() string { return runningSHA }
			res, err := Run(context.Background(), io.Discard, true)
			wantCompared := []string{runningSHA + "..." + staleSHA, runningSHA + "..." + nextSHA}
			if !errors.Is(err, ErrOutdated) || res.Latest != nextTag || !slices.Equal(compared, wantCompared) || !slices.Equal(requested, []string{nextTag}) {
				t.Fatalf("result=%+v err=%v compared=%v requested=%v", res, err, compared, requested)
			}
		})
	}
}
