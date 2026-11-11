package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPullRevisionRejectsUnusableResponses(t *testing.T) {
	failure := errors.New("gh failed")
	for _, tc := range []struct {
		name string
		raw  string
		etag string
		err  error
	}{
		{name: "304 without cache", raw: "HTTP/2.0 304 Not Modified\r\n\r\n", err: failure},
		{name: "transport error", etag: "old", err: failure},
		{name: "malformed response", raw: "not HTTP", etag: "old"},
		{name: "failed 200", raw: "HTTP/2.0 200 OK\r\n\r\n", etag: "old", err: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(context.Context, ...string) ([]byte, error) { return []byte(tc.raw), tc.err }
			if _, unchanged, err := readRepoPullRevisionWithGH(t.Context(), run, repo, tc.etag); err == nil || unchanged {
				t.Fatalf("unusable response accepted: unchanged=%t err=%v", unchanged, err)
			}
		})
	}
}

func TestUnmergedRevision304ReusesCachedPulls(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	now := r.a.timeNow()
	l.unmerged = map[string]loopUnmergedObservation{
		repo: {etag: "old", at: now.Add(-unmergedCacheTTL), fullAt: now.Add(-10 * time.Minute),
			pulls: []unmergedPull{{Number: 7, CreatedAt: now.Add(-time.Hour)}}},
	}
	readRepoOpenPulls = func(context.Context, string) ([]unmergedPull, error) {
		t.Fatal("304 caused a full inventory read")
		return nil, nil
	}
	readRepoPullRevision = func(ctx context.Context, target, etag string) (string, bool, error) {
		return readRepoPullRevisionWithGH(ctx, func(_ context.Context, args ...string) ([]byte, error) {
			if !slices.Contains(args, "If-None-Match: old") {
				t.Fatalf("missing cached validator: %v", args)
			}
			return []byte("HTTP/2.0 304 Not Modified\r\n\r\n"), errors.New("exit status 1: gh: HTTP 304")
		}, target, etag)
	}
	pulls, _, err := l.repoRecentUnmerged(t.Context(), repo)
	if err != nil || len(pulls) != 1 || pulls[0].Number != 7 || !l.unmerged[repo].at.Equal(now) {
		t.Fatalf("cached inventory not refreshed: pulls=%v err=%v observation=%+v", pulls, err, l.unmerged[repo])
	}
}

func TestRefillContinuesAfterRepositoryObservationFailure(t *testing.T) {
	for _, globalHold := range []bool{false, true} {
		t.Run(fmt.Sprintf("global hold %t", globalHold), func(t *testing.T) {
			r, busy := fillRig(t)
			original := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == loopBoard && !slices.Contains(args, "o/unavailable#9") {
					return `{"pick":{"ref":"o/unavailable#9","url":"https://github.com/o/unavailable/issues/9"}}`, 0
				}
				if args[0] == agentsName && args[1] == agentStartName && args[2] == "unavailable 9" {
					t.Fatal("started work without a repository observation")
				}
				return original(args)
			}
			readRepoPullRevision = func(_ context.Context, target, _ string) (string, bool, error) {
				if target == "o/unavailable" {
					if globalHold {
						return "", false, fmt.Errorf("budget held: %w", errLoopLater)
					}
					return "", false, errors.New("repository unavailable")
				}
				return "", false, nil
			}
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if err := l.fill(t.Context(), st, nil, nil); err != nil {
				t.Fatal(err)
			}
			want := 3
			if globalHold {
				want = 0
			}
			if *busy != want || !strings.Contains(r.out.String(), "open pull requests") {
				t.Fatalf("busy=%d want=%d; %s", *busy, want, r.out)
			}
			if deferred := slices.Contains(l.fillSkipped(), "o/unavailable#9"); deferred == globalHold {
				t.Fatalf("repository deferral=%t, global hold=%t", deferred, globalHold)
			}
		})
	}
}
