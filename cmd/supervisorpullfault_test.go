package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
)

// gatewayTimeoutPage is the start of GitHub's HTML page for a 504.
const gatewayTimeoutPage = "<!DOCTYPE html>\n<html>\n  <head>\n    <title>Unicorn! &middot; GitHub</title>"

// errPullBatch504 is gh's error for GitHub's 504, once its tries are spent.
var errPullBatch504 = errors.New("gh api graphql (rateLimit): HTTP 504 after 3 tries: gh api graphql -f: exit status 1: gh: HTTP 504")

// faultPullBatches answers the pull batches fail says fail as GitHub
// answers a query it cannot finish: gh's 504 with the HTML page. The others
// are read from the fixture. It returns the sizes of the batches asked.
func faultPullBatches(t *testing.T, fail func(query string, size int) bool) *[]int {
	t.Helper()
	fakePullGraphQL(t, func(query string, _ int) map[string]any {
		return pullGraphQLFixture(t, query, loop.CheckSuccess, loopHead, watchFixtureUpdated)
	})
	answer := github.RunGH
	sizes := new([]int)
	github.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		query := args[len(args)-1]
		n := strings.Count(query, ": repository(")
		*sizes = append(*sizes, n)
		if fail(query, n) {
			return []byte(gatewayTimeoutPage), errPullBatch504
		}
		return answer(ctx, args...)
	}
	return sizes
}

func pullRefs(n int) []string {
	refs := make([]string, n)
	for i := range refs {
		refs[i] = fmt.Sprintf("o/r#%d", i+1)
	}
	return refs
}

// A batch GitHub cannot answer in time is asked again in halves, and the
// halved size holds for the polls after it: the restarted loop's first
// batch of every pull request on first sight got a 504 with GitHub's HTML
// page at every try, and the watch read none of them for a day (#3809).
func TestPullWatcherHalvesABatchGitHubFailsWithItsHTMLPage(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	sizes := faultPullBatches(t, func(_ string, n int) bool { return n > 6 })
	refs := pullRefs(12)
	w := l.PullWatcher()
	w.Snapshot.ListedAt = time.Now()
	w.Set(refs, nil, nil, 0, nil)
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatalf("poll: %v\n%s", err, r.out.String())
	}
	if !slices.Equal(*sizes, []int{12, 6, 6}) {
		t.Errorf("batches asked: %v, want 12, then 6 and 6", *sizes)
	}
	v := w.View()
	for _, ref := range refs {
		if p, err := w.Read(ref, time.Now()); err != nil || !p.Pull.Green {
			t.Errorf("%s: %+v %v", ref, p, err)
		}
	}
	if v.PolledAt.IsZero() || v.Failing != nil {
		t.Errorf("complete poll: polled at %v, failing %+v", v.PolledAt, v.Failing)
	}
	out := r.out.String()
	for _, want := range []string{"GitHub failed a batch of 12 pull requests", "HTTP 504", "<!DOCTYPE html> <html> <head> <title>Unicorn!", "asking again in batches of 6"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "invalid character") {
		t.Errorf("the error is the JSON decoder's, not GitHub's:\n%s", out)
	}
	// The next poll asks for the halved size from the start.
	*sizes = nil
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*sizes, []int{6, 6}) {
		t.Errorf("batches asked after the halving: %v, want 6 and 6", *sizes)
	}
}

// Pull requests on first sight are read twenty to a batch, those seen
// before sixty: details are the heaviest query the watch asks.
func TestPullWatcherReadsFirstSightsInSmallerBatches(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	sizes := faultPullBatches(t, func(string, int) bool { return false })
	w := l.PullWatcher()
	w.Snapshot.ListedAt = time.Now()
	w.Set(pullRefs(70), nil, nil, 0, nil)
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if want := []int{20, 20, 20, 10}; !slices.Equal(*sizes, want) {
		t.Errorf("first sight: batches %v, want %v", *sizes, want)
	}
	*sizes = nil
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if want := []int{60, 10}; !slices.Equal(*sizes, want) {
		t.Errorf("seen before: batches %v, want %v", *sizes, want)
	}
}

// A batch GitHub keeps failing at the smallest size is skipped: the rest
// are read and stand, and the poll fails, naming GitHub's answer.
func TestPullWatcherReadsTheRestPastABatchGitHubKeepsFailing(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	sizes := faultPullBatches(t, func(query string, _ int) bool { return strings.Contains(query, "pullRequest(number:3)") })
	w := l.PullWatcher()
	w.Snapshot.ListedAt = time.Now()
	w.Set(pullRefs(8), nil, nil, 0, nil)
	_, err = w.Poll(t.Context(), l)
	var fault *loop.PullBatchFault
	if !errors.As(err, &fault) || !strings.Contains(err.Error(), "HTTP 504") {
		t.Fatalf("poll: %v", err)
	}
	if want := []int{8, loop.LoopPullMinBatch, 3}; !slices.Equal(*sizes, want) {
		t.Errorf("batches %v, want %v", *sizes, want)
	}
	v := w.View()
	refs := pullRefs(8)
	for _, ref := range refs[5:] {
		if _, err := w.Read(ref, time.Now()); err != nil {
			t.Errorf("%s not read past the failed batch: %v", ref, err)
		}
	}
	if _, ok := v.Pulls[refs[2]]; ok || !v.PolledAt.IsZero() {
		t.Errorf("the failed batch counts as read: %+v, polled at %v", v.Pulls[refs[2]], v.PolledAt)
	}
	if !strings.Contains(r.out.String(), "reading the rest") {
		t.Errorf("skip unsaid:\n%s", r.out.String())
	}
	// The next poll refreshes what it read, then asks for the rest again.
	*sizes = nil
	_, _ = w.Poll(t.Context(), l)
	if want := []int{3, loop.LoopPullMinBatch}; !slices.Equal(*sizes, want) {
		t.Errorf("next poll's batches: %v, want %v", *sizes, want)
	}
}

// What GitHub fails to answer, after gh's tries, is a fault a smaller
// batch may pass, named by GitHub's answer; what it refuses is not.
func TestPullBatchFaultIsGitHubFailingNotRefusing(t *testing.T) {
	for name, tc := range map[string]struct {
		body  string
		err   error
		fault bool
		says  []string
	}{
		"html 504":        {body: gatewayTimeoutPage, err: errors.New("gh api graphql -f: exit status 1: gh: HTTP 504"), fault: true, says: []string{"GitHub answered no JSON", "Unicorn!", "HTTP 504 after 3 tries"}},
		"html exit 0":     {body: "<html>proxy</html>", fault: true, says: []string{"GitHub answered no JSON", "<html>proxy</html>", "a cut-off body after 3 tries"}},
		"empty 502":       {err: errors.New("gh api graphql -f: exit status 1: gh: HTTP 502"), fault: true, says: []string{"HTTP 502 after 3 tries"}},
		"empty exit 0":    {fault: true, says: []string{"an empty body after 3 tries"}},
		"graphql timeout": {body: `{"data":null,"errors":[{"message":"Something went wrong while executing your query. This may be the result of a timeout, or it could be a GitHub bug."}]}`, fault: true, says: []string{"result of a timeout"}},
		"secondary 403":   {body: `{"message":"You have exceeded a secondary rate limit."}`, err: errors.New("gh api graphql -f: exit status 1: gh: You have exceeded a secondary rate limit. (HTTP 403)"), says: []string{"secondary rate limit"}},
		"graphql error":   {body: `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`, says: []string{"not accessible"}},
	} {
		t.Run(name, func(t *testing.T) {
			old := github.RunGH
			t.Cleanup(func() { github.RunGH = old })
			github.RunGH = github.Retrying(func(context.Context, ...string) ([]byte, error) {
				return []byte(tc.body), tc.err
			}, time.Millisecond)
			_, err := loop.FetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
			var fault *loop.PullBatchFault
			if err == nil || errors.As(err, &fault) != tc.fault {
				t.Fatalf("err %v (fault %v), want fault %v", err, errors.As(err, &fault), tc.fault)
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err %v lacks %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "invalid character") {
				t.Errorf("the JSON decoder's error, not GitHub's answer: %v", err)
			}
		})
	}
}

// The watch says every failed try, with its count and the next try, and
// the poll that ends the episode; supervisor status reads the episode.
func TestPullWatcherSaysEveryFailedTryAndItsRecovery(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	sizes := faultPullBatches(t, func(string, int) bool { return true })
	w := l.PullWatcher()
	w.Snapshot.ListedAt = time.Now()
	w.Set([]string{loopPR}, nil, nil, 0, nil)
	schedule := loop.LoopPullSchedule{}
	for i := 1; i <= 3; i++ {
		next, err := w.Poll(t.Context(), l)
		if got := schedule.After(l, next, err); got != loop.PullWatchRetry(i) {
			t.Fatalf("try %d: next in %s", i, got)
		}
		if !strings.Contains(r.out.String(), fmt.Sprintf("SUPERVISOR PULL WATCH held: try %d failed", i)) {
			t.Fatalf("try %d unsaid:\n%s", i, r.out.String())
		}
	}
	var saved loop.LoopPullSnapshot
	if ok, err := r.a.store.ReadFile(loop.LoopPullWatchFile, &saved); !ok || err != nil || saved.Failing == nil || saved.Failing.Tries != 3 {
		t.Fatalf("saved episode: %+v %v %v", saved.Failing, ok, err)
	}
	if line := saved.FailingLine(time.Now()); !strings.Contains(line, "3 tries (last complete poll never)") || !strings.Contains(line, "HTTP 504") {
		t.Errorf("status line: %s", line)
	}
	if held, pulls := r.a.loopWaitLines(); !strings.Contains(pulls, "pull watch failing since") {
		t.Errorf("supervisor status lacks the episode: %q %q", held, pulls)
	}
	github.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		*sizes = append(*sizes, 1)
		return []byte(`{"data":{"rateLimit":{"remaining":4900},"p0":{"pullRequest":null}}}`), nil
	}
	next, err := w.Poll(t.Context(), l)
	if err != nil || schedule.After(l, next, err) != loop.LoopPullLong || schedule.Failures != 0 {
		t.Fatalf("did not recover: %v", err)
	}
	if n := strings.Count(r.out.String(), "SUPERVISOR PULL WATCH recovered: the poll reads the desk's pull requests again after 3 failed tries"); n != 1 {
		t.Errorf("recovery said %d times:\n%s", n, r.out.String())
	}
	var recovered loop.LoopPullSnapshot
	if ok, err := r.a.store.ReadFile(loop.LoopPullWatchFile, &recovered); !ok || err != nil || recovered.Failing != nil || recovered.PolledAt.IsZero() {
		t.Errorf("saved after recovery: failing %+v, polled at %v", recovered.Failing, recovered.PolledAt)
	}
}

// The continuous watch that has not read all the desk's pull requests for
// half an hour raises one alarm for the guide, naming its failed tries,
// and says when a poll completes again. A pass's single poll raises none.
func TestPullStallAlarmsOncePerEpisodeAndSaysItsEnd(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	w := l.PullWatcher()
	w.Snapshot.PolledAt = r.a.now.Add(-2 * time.Hour)
	if err := l.CheckPullStall(); err != nil || len(r.logged(t, "supervisor.file")) != 0 {
		t.Fatalf("a watch that never started raised an alarm: %v", err)
	}
	w.Started = r.a.now.Add(-loop.LoopPullStallAfter + time.Minute)
	w.Snapshot.Failing = &loop.LoopPullFailing{Since: w.Started, Tries: 7, Error: "pull batch: GitHub answered no JSON: HTTP 504"}
	if err := l.CheckPullStall(); err != nil || len(r.logged(t, "supervisor.file")) != 0 {
		t.Fatalf("alarm within half an hour of the watch's start: %v", err)
	}
	r.a.now = r.a.now.Add(2 * time.Minute)
	for range 2 {
		if err := l.CheckPullStall(); err != nil {
			t.Fatal(err)
		}
	}
	alarms := r.logged(t, "supervisor.file")
	if len(alarms) != 1 {
		t.Fatalf("alarms: %d, want one per episode", len(alarms))
	}
	for _, want := range []string{"The pull watch has not read all the desk's pull requests since", "7 tries failed", "HTTP 504", "no desk review, merge decision or CI wake"} {
		if !strings.Contains(alarms[0].Detail, want) {
			t.Errorf("alarm lacks %q: %s", want, alarms[0].Detail)
		}
	}
	w.Snapshot.PolledAt, w.Snapshot.Failing = r.a.now, nil
	if err := l.CheckPullStall(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(r.out.String(), "SUPERVISOR STALL ENDED: the pull watch reads the desk's pull requests again"); n != 1 {
		t.Errorf("end said %d times:\n%s", n, r.out.String())
	}
	// A new episode raises a new alarm.
	r.a.now = r.a.now.Add(loop.LoopPullStallAfter)
	if err := l.CheckPullStall(); err != nil || len(r.logged(t, "supervisor.file")) != 2 {
		t.Fatalf("second episode: %v, %d alarms", err, len(r.logged(t, "supervisor.file")))
	}
	if !strings.Contains(r.logged(t, "supervisor.file")[1].Detail, "no poll has finished since") {
		t.Errorf("a watch without failed tries: %s", r.logged(t, "supervisor.file")[1].Detail)
	}
}

// A new listing stands with a poll that read what it listed: a pass takes
// each listed pull request the watch did not read as unread, so a listing
// a failed poll committed made every pass carry the desk's whole list.
func TestPullWatcherKeepsANewListingOnlyWithAPollThatReadIt(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	listed, batch := loop.ReadOpenPulls, loop.ReadLoopPullBatch
	t.Cleanup(func() { loop.ReadOpenPulls, loop.ReadLoopPullBatch = listed, batch })
	refs := pullRefs(3)
	loop.ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) { return refs, nil }
	fail := true
	loop.ReadLoopPullBatch = func(_ context.Context, asked []string, _ map[string]loop.WatchedLoopPull, _ int) (map[string]loop.WatchedLoopPull, error) {
		if fail {
			return nil, errors.New("gh: Bad credentials (HTTP 401)")
		}
		out := map[string]loop.WatchedLoopPull{}
		for _, ref := range asked {
			out[ref] = loop.WatchedLoopPull{Pull: r.pull, At: time.Now()}
		}
		return out, nil
	}
	w := l.PullWatcher()
	w.Set(nil, []string{loopDesk}, []string{"o"}, 0, nil)
	if _, err := w.Poll(t.Context(), l); err == nil {
		t.Fatal("a failed batch passed")
	}
	if v := w.View(); len(v.Open) != 0 || !v.ListedAt.IsZero() {
		t.Fatalf("a failed poll kept its listing: open %v, listed at %v", v.Open, v.ListedAt)
	}
	fail = false
	if _, err := w.Poll(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if v := w.View(); !slices.Equal(v.Open, refs) || v.ListedAt.IsZero() || len(v.Pulls) != len(refs) {
		t.Fatalf("a complete poll's listing: open %v, listed at %v, %d observations", v.Open, v.ListedAt, len(v.Pulls))
	}
}
