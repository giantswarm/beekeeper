package superloop

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	me       = "me"
	ourTeam  = "team-a"
	teamB    = "team-b"
	upstream = "up/stream#1"
	tool     = "o/tool"
	// bob is a member of the owning team.
	bob = "bob"
)

// desk is a desk of team-a in org o, whose loop first started an hour
// before t0.
func desk() Desk {
	return Desk{Since: t0.Add(-time.Hour), Orgs: []string{"o"}, Authors: []string{me}, Team: ourTeam,
		Review: []string{"o/*-configs"}, Never: []string{tool}, OwnerWait: 24 * time.Hour}
}

// owned is pr opened by the desk, its repository owned by team.
func owned(team string, reviews ...Review) map[string]Pull {
	return map[string]Pull{pr: {Ref: pr, URL: prURL, State: Open, Head: head1, Green: true, Author: me, Team: team, Reviews: reviews}}
}

func kinds(acts []Action) []string {
	var out []string
	for _, a := range acts {
		out = append(out, a.Kind)
	}
	return out
}

// left is the one leave in acts, which must start no review and give no go.
func left(t *testing.T, acts []Action, reason string) Action {
	t.Helper()
	a := only(t, acts, KindLeave)
	if !strings.HasSuffix(a.Key, " "+reason) {
		t.Errorf("leave = %+v, want reason %s", a, reason)
	}
	return a
}

func TestAParkOutsideTheDesksOrgsIsLeftAloneUnread(t *testing.T) {
	w := []Worker{{Name: worker, ParkedOn: upstream, ParkedAt: t0}}
	if got := Tracked(w, nil, nil, desk()); len(got) != 0 {
		t.Errorf("an upstream pull request is read: %v", got)
	}
	in := Input{Workers: w, Rounds: 1, Desk: desk(), Now: t0}
	a := left(t, Plan(in), leftOutside)
	if a.Agent != worker || a.Ref != upstream || !strings.Contains(a.Message, "outside github.allowedOrgs") {
		t.Errorf("leave = %+v", a)
	}
	// Said once.
	in.Taken = taken(t, t0, a)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a second pass took %+v", got)
	}
	// Its reviewer, started before the desk knew better, retires.
	reviewer := Worker{Name: ReviewerName(upstream, 1), Started: true}
	in.Workers = append(w, reviewer)
	in.Taken = append(taken(t, t0, Action{Key: reviewKey(upstream, head1), Kind: KindReview, Ref: upstream, Agent: worker}), taken(t, t1, a)...)
	if got := Tracked(in.Workers, in.Taken, nil, desk()); len(got) != 0 {
		t.Errorf("a reviewed upstream pull request is read: %v", got)
	}
	if d := only(t, Plan(in), KindDone); d.Agent != reviewer.Name {
		t.Errorf("done = %+v", d)
	}
}

func TestANeverRepositoryIsLeftAloneWhateverElseSays(t *testing.T) {
	d := desk()
	d.Review = append(d.Review, "o/*")
	w := []Worker{{Name: worker, ParkedOn: tool + "#1", ParkedAt: t0}}
	if got := Tracked(w, nil, nil, d); len(got) != 0 {
		t.Errorf("a never repository is read: %v", got)
	}
	left(t, Plan(Input{Workers: w, Rounds: 1, Desk: d, Now: t0}), leftNever)
	// A pull request elsewhere that names it: its title, its branch or a
	// path it changes, overridden or not.
	for _, p := range []Pull{{Title: "chore: update tool (team-b)"}, {Branch: "reposetup/update-tool"}, {Files: []string{"tool/values.yaml"}}} {
		pulls := owned(ourTeam)
		q := pulls[pr]
		q.Title, q.Branch, q.Files = p.Title, p.Branch, p.Files
		pulls[pr] = q
		in := Input{Workers: parked(t0), Pulls: pulls, Rounds: 1, Desk: d, Now: t0,
			Taken: taken(t, t0, Action{Key: overrideKey(pr), Kind: KindOverride, Ref: pr})}
		if a := left(t, Plan(in), leftNever); !strings.Contains(a.Message, tool) {
			t.Errorf("leave = %+v", a)
		}
	}
	// A name inside a longer one names nothing.
	pulls := owned(ourTeam)
	q := pulls[pr]
	q.Title, q.Files = "fix the toolbox", []string{".toolrc", "tools/x"}
	pulls[pr] = q
	only(t, Plan(Input{Workers: parked(t0), Pulls: pulls, Rounds: 1, Desk: d, Now: t0}), KindReview)
}

func TestAParkFromBeforeTheLoopsFirstStartIsLeftAlone(t *testing.T) {
	d := desk()
	d.Since = t0
	early := parked(t0.Add(-48 * time.Hour))
	if got := Tracked(early, nil, nil, d); len(got) != 0 {
		t.Errorf("an early park is read: %v", got)
	}
	a := left(t, Plan(Input{Workers: early, Rounds: 1, Desk: d, Now: t0}), leftBefore)
	if !strings.Contains(a.Message, "before the loop's first start") {
		t.Errorf("leave = %+v", a)
	}
	// Even with a round the loop started on it before it knew better: no
	// go on its verdict.
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	rev := Review{ID: "R1", Author: me, Head: head1, Verdict: Approved, At: t1}
	if got := Plan(Input{Workers: early, Pulls: owned(ourTeam, rev), Rounds: 1, Desk: d, Now: t1, Taken: tk}); slices.Contains(kinds(got), KindGo) {
		t.Errorf("an early park got %+v", got)
	}
	// A park from after it is reviewed.
	only(t, Plan(Input{Workers: parked(t0.Add(time.Minute)), Pulls: owned(ourTeam), Rounds: 1, Desk: d, Now: t1}), KindReview)
	// So is one a second worker made since, beside an early one: the
	// later park is the request.
	both := append(early, Worker{Name: "worker two", ParkedOn: pr, ParkedAt: t0.Add(time.Minute)})
	if got := Tracked(both, nil, nil, d); len(got) != 1 {
		t.Errorf("a pull request parked on since is not read: %v", got)
	}
	if a := only(t, Plan(Input{Workers: both, Pulls: owned(ourTeam), Rounds: 1, Desk: d, Now: t1}), KindReview); a.Agent != "worker two" {
		t.Errorf("review = %+v", a)
	}
}

func TestAPullRequestSomebodyElseOpenedIsLeftAlone(t *testing.T) {
	pulls := owned(ourTeam)
	q := pulls[pr]
	q.Author = "colleague"
	pulls[pr] = q
	a := left(t, Plan(Input{Workers: parked(t0), Pulls: pulls, Rounds: 1, Desk: desk(), Now: t0}), leftAuthor)
	if !strings.Contains(a.Message, "opened by colleague") {
		t.Errorf("leave = %+v", a)
	}
	q.Author = "ME"
	pulls[pr] = q
	only(t, Plan(Input{Workers: parked(t0), Pulls: pulls, Rounds: 1, Desk: desk(), Now: t0}), KindReview)
}

func TestTheDesksOwnAndSharedRepositoriesAreReviewedAsToday(t *testing.T) {
	only(t, Plan(Input{Workers: parked(t0), Pulls: owned(ourTeam), Rounds: 1, Desk: desk(), Now: t0}), KindReview)
	// No team configured: every repository of the orgs is the desk's.
	d := desk()
	d.Team = ""
	only(t, Plan(Input{Workers: parked(t0), Pulls: owned(teamB), Rounds: 1, Desk: d, Now: t0}), KindReview)
	// A repository of the review list, whoever owns it.
	const shared = "o/x-configs#3"
	p := owned(teamB)[pr]
	p.Ref = shared
	w := []Worker{{Name: worker, ParkedOn: shared, ParkedAt: t0}}
	a := only(t, Plan(Input{Workers: w, Pulls: map[string]Pull{shared: p}, Rounds: 1, Desk: desk(), Now: t0}), KindReview)
	if a.Ref != shared {
		t.Errorf("review = %+v", a)
	}
	// And the go on its approval, as today.
	rev := Review{ID: "R1", Author: me, Head: head1, Verdict: Approved, At: t1}
	p.Reviews = []Review{rev}
	only(t, Plan(Input{Workers: w, Pulls: map[string]Pull{shared: p}, Rounds: 1, Desk: desk(), Now: t1, Taken: taken(t, t0, a)}), KindGo)
}

func TestAnotherTeamsPullRequestGetsNoReviewerNorGo(t *testing.T) {
	in := Input{Workers: parked(t0), Pulls: owned(teamB), Rounds: 1, Desk: desk(), Now: t0.Add(time.Hour)}
	a := left(t, Plan(in), leftTeam)
	if !strings.Contains(a.Message, "team-b owns o/r") {
		t.Errorf("leave = %+v", a)
	}
	// No team file declares it: nobody's of the desk's either.
	in.Pulls = owned("")
	if a := left(t, Plan(in), leftTeam); !strings.Contains(a.Message, "no team file declares o/r") {
		t.Errorf("leave = %+v", a)
	}
	// A round the loop started before it knew better gives no go, and the
	// leave retires its reviewer.
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	rev := Review{ID: "R1", Author: me, Head: head1, Verdict: Approved, At: t1}
	in.Workers = append(parked(t0), Worker{Name: ReviewerName(pr, 1), Started: true})
	in.Pulls = owned(teamB, rev)
	in.Taken = taken(t, t0, started)
	got := Plan(in)
	if !slices.Equal(kinds(got), []string{KindLeave}) {
		t.Fatalf("plan = %+v", got)
	}
	in.Taken = append(in.Taken, taken(t, t1, got[0])...)
	if d := only(t, Plan(in), KindDone); d.Agent != ReviewerName(pr, 1) {
		t.Errorf("done = %+v", d)
	}
}

func TestTheOwningTeamsReviewReachesItsWorker(t *testing.T) {
	ours := Review{ID: "R0", Author: me, Head: head1, Body: "Review: approved", At: t1}
	bot := Review{ID: "R1", Author: "ci[bot]", Head: head1, Body: "nit", At: t1}
	before := Review{ID: "R2", Author: bob, Head: head1, Verdict: ChangesRequested, Body: "old", At: t0.Add(-time.Minute)}
	theirs := Review{ID: "R3", Author: bob, Head: head1, Verdict: ChangesRequested, Body: "rename x", At: t1}
	in := Input{Workers: parked(t0), Pulls: owned(teamB, ours, bot, before, theirs), Rounds: 1, Desk: desk(), Now: t1,
		Taken: taken(t, t0, Action{Key: leaveKey(pr, leftTeam), Kind: KindLeave, Ref: pr, Agent: worker})}
	a := only(t, Plan(in), KindRelay)
	if a.Agent != worker || !strings.Contains(a.Message, "rename x") || !strings.Contains(a.Message, "bob reviewed") || !strings.Contains(a.Message, "team-b") {
		t.Errorf("relay = %+v", a)
	}
	in.Taken = append(in.Taken, taken(t, t1, a)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("relayed twice: %+v", got)
	}
}

func TestABlockingOwnerReviewGoesToThePersonOnce(t *testing.T) {
	lv := Action{Key: leaveKey(pr, leftTeam), Kind: KindLeave, Ref: pr, Agent: worker}
	in := Input{Workers: parked(t0), Pulls: owned(teamB), Rounds: 1, Desk: desk(), Now: t0.Add(23 * time.Hour), Taken: taken(t, t0, lv)}
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("asked before ownerWait: %+v", got)
	}
	in.Now = t0.Add(25 * time.Hour)
	a := only(t, Plan(in), KindOwnership)
	if a.Agent != worker || a.Question != OwnershipQuestion(prURL, teamB) || !strings.HasPrefix(a.Question, "Override team-b's ownership of "+prURL) {
		t.Errorf("ownership = %+v", a)
	}
	// While the person has not answered, the pull request is not read.
	in.Taken = append(in.Taken, taken(t, in.Now, a)...)
	if got := Tracked(in.Workers, in.Taken, nil, in.Desk); len(got) != 0 {
		t.Errorf("tracked = %v", got)
	}
	if got := Plan(in); len(got) != 0 {
		t.Errorf("asked twice: %+v", got)
	}
	// An owning team that approved does not block.
	in.Taken = taken(t, t0, lv)
	in.Pulls = owned(teamB, Review{ID: "R1", Author: bob, Head: head1, Verdict: Approved, At: t0.Add(-time.Minute)})
	if got := Plan(in); len(got) != 0 {
		t.Errorf("an approved pull request got %+v", got)
	}
}

func TestAnOverrideMakesThatPullRequestTheDesks(t *testing.T) {
	asked := Action{Key: ownershipKey(pr), Kind: KindOwnership, Ref: pr, Agent: worker}
	tk := append(taken(t, t0, Action{Key: leaveKey(pr, leftTeam), Kind: KindLeave, Ref: pr, Agent: worker}), taken(t, t1, asked)...)
	answer := func(text string) []Answer {
		return []Answer{{Ref: pr, Note: 9, Question: OwnershipQuestion(prURL, teamB), Text: text, By: "pat", At: t1.Add(time.Hour)}}
	}
	in := Input{Workers: parked(t0), Pulls: owned(teamB), Rounds: 1, Desk: desk(), Now: t1.Add(2 * time.Hour), Taken: tk, Answers: answer("Override")}
	if got := Tracked(in.Workers, in.Taken, in.Answers, in.Desk); len(got) != 1 {
		t.Fatalf("an answered pull request is not tracked: %v", got)
	}
	o := only(t, Plan(in), KindOverride)
	if !strings.Contains(o.Message, "pat overrode team-b's ownership") {
		t.Errorf("override = %+v", o)
	}
	in.Taken = append(in.Taken, taken(t, in.Now, o)...)
	only(t, Plan(in), KindReview)
	// Never outranks it.
	in.Desk.Never = append(in.Desk.Never, "o/r")
	left(t, Plan(in), leftNever)

	// Any other answer goes to the worker word for word, once.
	in = Input{Workers: parked(t0), Pulls: owned(teamB), Rounds: 1, Desk: desk(), Now: t1.Add(2 * time.Hour), Taken: tk, Answers: answer("Wait — ask Bob on Slack first")}
	a := only(t, Plan(in), KindAnswer)
	if a.Agent != worker || !strings.Contains(a.Message, "ask Bob on Slack first") || !strings.Contains(a.Message, "not read it as an override") {
		t.Errorf("answer = %+v", a)
	}
	in.Taken = append(in.Taken, taken(t, in.Now, a)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a second pass took %+v", got)
	}
}

func TestOverrides(t *testing.T) {
	for answer, want := range map[string]bool{
		"Override":                        true,
		"override.":                       true,
		"Override — the desk reviews it":  true,
		"Override: unless team-b objects": false,
		"ok, override it":                 true,
		"Wait: the worker stays parked":   false,
		"don't override":                  false,
		"override once team-b had a look": false,
		"":                                false,
	} {
		if got := Overrides(answer); got != want {
			t.Errorf("Overrides(%q) = %v, want %v", answer, got, want)
		}
	}
}

func TestNameWord(t *testing.T) {
	for text, want := range map[string]bool{
		"chore(repositories): update nancy-fixer (team-shield)": true,
		"reposetup/update-nancy-fixer":                          true,
		"nancy-fixer/values.yaml":                               true,
		".nancy-ignore":                                         false,
		"nancy-fixers":                                          false,
		"xnancy-fixer":                                          false,
	} {
		if got := nameWord("o/nancy-fixer").MatchString(text); got != want {
			t.Errorf("nameWord(nancy-fixer) on %q = %v, want %v", text, got, want)
		}
	}
	if !nameWord("o/nancy-*").MatchString("bump nancy-fixer") {
		t.Error("a pattern names nothing")
	}
}
