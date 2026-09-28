package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeGH answers the plain squash merge's gh calls: the pull request, its
// checks in turn (the last one repeats), the merge and the branch deletion.
type fakeGH struct {
	pull   string
	checks []string
	merge  error
	calls  []string
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "api user --jq .login":
		return []byte("teemow\n"), nil
	case strings.HasPrefix(call, "pr view"):
		return []byte(f.pull), nil
	case strings.HasPrefix(call, "pr checks"):
		c := f.checks[0]
		if len(f.checks) > 1 {
			f.checks = f.checks[1:]
		}
		if c == "none" {
			return nil, errors.New("gh pr checks: exit status 1: no checks reported on the 'x' branch")
		}
		return []byte(c), nil
	case strings.HasPrefix(call, "api -X PUT"):
		if f.merge != nil {
			return nil, f.merge
		}
		return []byte(`{"sha":"abcdef1234567","merged":true}`), nil
	case strings.HasPrefix(call, "api -X DELETE"):
		return nil, nil
	}
	return nil, errors.New("unexpected gh " + call)
}

const openPull = `{"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","headRefOid":"1234567890","headRefName":"fix","title":"fix: it","author":{"login":"teemow","is_bot":false}}`

func squash(f *fakeGH) SquashDoc {
	now := time.Unix(0, 0)
	return Squash{GH: f.run, Repo: "teemow/lab", Number: 7, Timeout: time.Minute, Poll: 30 * time.Second,
		Now: func() time.Time { return now }, Sleep: func(d time.Duration) { now = now.Add(d) }}.Run(context.Background())
}

// A green head is squash-merged with its title and number as the subject,
// the judged head as the expected one, and its branch deleted.
func TestSquashMergesAGreenHead(t *testing.T) {
	f := &fakeGH{pull: openPull, checks: []string{`[{"name":"ci","bucket":"pending"}]`, `[{"name":"ci","bucket":"pass"}]`}}
	doc := squash(f)
	if doc.ExitCode != SquashMerged || doc.MergeCommitSha != "abcdef1234567" || doc.Release != nil {
		t.Fatalf("doc %+v", doc)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"sha=1234567890", "commit_title=fix: it (#7)", "merge_method=squash", "api -X DELETE repos/teemow/lab/git/refs/heads/fix"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in the calls:\n%s", want, joined)
		}
	}
}

// What devctl refuses before its wait, the plain route refuses with the
// same codes, and it merges nothing on red or a timeout.
func TestSquashRefusesAndWaits(t *testing.T) {
	for name, tc := range map[string]struct {
		pull, checks string
		merge        error
		want         int
	}{
		"merged":    {pull: strings.Replace(openPull, "OPEN", "MERGED", 1), want: SquashNotApplicable},
		"draft":     {pull: strings.Replace(openPull, `"isDraft":false`, `"isDraft":true`, 1), want: SquashNotApplicable},
		"conflict":  {pull: strings.Replace(openPull, "MERGEABLE", "CONFLICTING", 1), want: SquashNotApplicable},
		"a person":  {pull: strings.Replace(openPull, `"login":"teemow"`, `"login":"someone"`, 1), want: SquashRefused},
		"a bot":     {pull: strings.Replace(openPull, `"login":"teemow","is_bot":false`, `"login":"renovate","is_bot":true`, 1), checks: `[]`, want: SquashMerged},
		"red":       {pull: openPull, checks: `[{"name":"ci","bucket":"fail"}]`, want: SquashRed},
		"timeout":   {pull: openPull, checks: `[{"name":"ci","bucket":"pending"}]`, want: SquashTimeout},
		"no checks": {pull: openPull, checks: "none", want: SquashMerged},
		"declined":  {pull: openPull, checks: `[]`, merge: errors.New("gh api: HTTP 405: blocked"), want: SquashNotApplicable},
		"broken":    {pull: openPull, checks: `[]`, merge: errors.New("gh api: dial tcp: timeout"), want: SquashTooling},
	} {
		f := &fakeGH{pull: tc.pull, checks: []string{tc.checks}, merge: tc.merge}
		if doc := squash(f); doc.ExitCode != tc.want || doc.Verdict == "" || doc.Reason == "" {
			t.Errorf("%s: %+v, want exit %d", name, doc, tc.want)
		}
		if tc.want != SquashMerged && tc.merge == nil && strings.Contains(strings.Join(f.calls, "\n"), "-X PUT") {
			t.Errorf("%s: merged anyway", name)
		}
	}
}
