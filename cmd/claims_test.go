package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	claimIssue  = "o/r#7"
	claimIssue2 = "o/r#8"
	deskOne     = "desk-one"
	deskTwo     = "desk-two"
)

// claimDesks are two desks, each with its own state and desk name, on one
// fake GitHub.
func claimDesks(t *testing.T) (one, two *app, gh *claimtest.GitHub) {
	t.Helper()
	gh = claimtest.New()
	was := claimGH
	claimGH = gh.GH
	t.Cleanup(func() { claimGH = was })
	desk := func(name string) *app {
		store, err := state.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{StateDir: store.Dir()}
		cfg.Board.Claims = config.BoardClaims{Desk: name, Refresh: config.Duration{Duration: 6 * time.Hour}, StaleAfter: config.Duration{Duration: 24 * time.Hour}}
		return &app{store: store, out: &bytes.Buffer{}, as: agentOne, now: time.Now(), cfg: cfg}
	}
	return desk(deskOne), desk(deskTwo), gh
}

// serveClaim records session name as serving issue on a's desk.
func serveClaim(t *testing.T, a *app, name, issue string) state.Party {
	t.Helper()
	p := state.Party{Session: "sess-" + strings.ReplaceAll(name, " ", "-"), Name: name}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records = append(st.Records, state.Record{Session: p, Issue: issue, At: time.Now().UTC()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func unserveClaim(t *testing.T, a *app, issue string) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, err := findRecord(st.Records, issue)
		if err == nil {
			st.Records = st.Records[:0]
		}
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
}

func markers(t *testing.T, gh *claimtest.GitHub, issue string) []claim.Marker {
	t.Helper()
	var out []claim.Marker
	for _, c := range gh.Comments(issue) {
		if m, ok := claim.Parse(c.Body); ok {
			out = append(out, m)
		}
	}
	return out
}

// A record is published as one marker comment of the desk, which the
// desk's syncs refresh in place, never adding another, and release when
// the record ends.
func TestSyncClaimsPublishesRefreshesAndReleasesOneComment(t *testing.T) {
	one, _, gh := claimDesks(t)
	ctx := context.Background()
	p := serveClaim(t, one, "Worker one", claimIssue)
	for range 2 {
		if err := one.syncClaims(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ms := markers(t, gh, claimIssue)
	if len(ms) != 1 || ms[0].Desk != deskOne || ms[0].Session != p.Session || !ms[0].Open() {
		t.Fatalf("markers = %+v", ms)
	}
	if gh.Calls["post"] != 1 || gh.Calls["edit"] != 0 {
		t.Errorf("calls = %v: a sync with nothing due writes nothing", gh.Calls)
	}

	// due for its refresh: the same comment is edited
	one.cfg.Board.Claims.Refresh.Duration = time.Nanosecond
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if ms := markers(t, gh, claimIssue); len(ms) != 1 || !ms[0].Seen.After(ms[0].At) && !ms[0].Seen.Equal(ms[0].At) {
		t.Errorf("refreshed markers = %+v", ms)
	}
	if gh.Calls["post"] != 1 || gh.Calls["edit"] != 1 {
		t.Errorf("calls = %v, want the refresh as an edit", gh.Calls)
	}

	// the record ends: the claim is released in place
	unserveClaim(t, one, claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	ms = markers(t, gh, claimIssue)
	if len(ms) != 1 || ms[0].Open() {
		t.Fatalf("released markers = %+v", ms)
	}
	st, _ := one.store.Read()
	if len(st.Claims) != 0 {
		t.Errorf("state keeps released claims %+v", st.Claims)
	}

	// claimed again: a new comment, so its id says when the claim opened,
	// and the desk's released one is deleted
	released := gh.Comments(claimIssue)[0].ID
	serveClaim(t, one, "Worker two", claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	cs := gh.Comments(claimIssue)
	if ms := markers(t, gh, claimIssue); len(ms) != 1 || !ms[0].Open() || cs[0].ID <= released || gh.Calls["post"] != 2 || gh.Calls["delete"] != 1 {
		t.Errorf("reclaimed markers = %+v, comments %+v, calls %v", ms, cs, gh.Calls)
	}
}

// A deleted comment is posted again; an idle agent's or ended session's
// record claims nothing.
func TestSyncClaimsRepostsAGoneCommentAndSkipsIdleRecords(t *testing.T) {
	one, _, gh := claimDesks(t)
	ctx := context.Background()
	serveClaim(t, one, "Worker one", claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	gh.Delete(gh.Comments(claimIssue)[0].ID)
	one.cfg.Board.Claims.Refresh.Duration = time.Nanosecond
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if ms := markers(t, gh, claimIssue); len(ms) != 1 || !ms[0].Open() {
		t.Errorf("markers after the comment's deletion = %+v", ms)
	}

	idle := serveClaim(t, one, "Idle one", claimIssue2)
	if err := one.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: idle})
		st.Records = append(st.Records, state.Record{Session: state.Party{Session: "ended-session", Name: "Ended"}, Issue: "p/q#9", Ended: time.Now()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if ms := markers(t, gh, claimIssue2); len(ms) != 0 {
		t.Errorf("an idle agent's record claimed %+v", ms)
	}
	if ms := markers(t, gh, "p/q#9"); len(ms) != 0 {
		t.Errorf("an ended session's record claimed %+v", ms)
	}
}

// Without board.claims.desk nothing is published: no machine name reaches
// an issue.
func TestSyncClaimsWithoutADeskPublishesNothing(t *testing.T) {
	one, _, gh := claimDesks(t)
	one.cfg.Board.Claims.Desk = ""
	one.cfg.Identity.Host = "a-machine"
	serveClaim(t, one, "Worker one", claimIssue)
	if err := one.syncClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.Comments(claimIssue)) != 0 || len(gh.Calls) != 0 {
		t.Errorf("published %v, calls %v", gh.Comments(claimIssue), gh.Calls)
	}
}

func claimCands(refs ...string) []board.Candidate {
	out := make([]board.Candidate, len(refs))
	for i, r := range refs {
		out[i] = board.Candidate{Item: board.Item{Ref: r}, Step: stepNextUp}
	}
	return out
}

// board next on one desk skips the item the other desk claims, with the
// reason, and picks the next; a stale claim is shown on the pick, not
// honoured.
func TestBoardNextSkipsAnItemAnotherDeskClaims(t *testing.T) {
	one, two, gh := claimDesks(t)
	ctx := context.Background()
	serveClaim(t, one, "Worker one", claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	st := &state.State{}
	sc := pickScope{me: state.Party{Name: agentOne}, alive: func(state.Party) bool { return false }, listed: time.Now()}

	cands := claimCands(claimIssue, claimIssue2)
	two.skipClaimed(ctx, io.Discard, cands, func() *board.Candidate { return nextFree(st, cands, sc).Pick })
	res := nextFree(st, cands, sc)
	if res.Pick == nil || res.Pick.Ref != claimIssue2 {
		t.Fatalf("pick = %+v, want %s", res.Pick, claimIssue2)
	}
	if len(res.Skipped) != 1 || !strings.HasPrefix(res.Skipped[0].Skip, claimedPrefix+deskOne) || res.SkippedBy[kindClaimed] != 1 {
		t.Errorf("skipped = %+v, by %v", res.Skipped, res.SkippedBy)
	}
	if gh.Calls["list"] > 3 {
		t.Errorf("calls = %v: only the candidates up to the pick are read", gh.Calls)
	}

	// the desk's own claim is no obstacle to itself
	cands = claimCands(claimIssue)
	one.skipClaimed(ctx, io.Discard, cands, func() *board.Candidate { return nextFree(st, cands, sc).Pick })
	if cands[0].Skip != "" {
		t.Errorf("own claim: skip %q", cands[0].Skip)
	}

	// stale: unrefreshed past staleAfter
	two.cfg.Board.Claims.StaleAfter.Duration = time.Nanosecond
	cands = claimCands(claimIssue)
	two.skipClaimed(ctx, io.Discard, cands, func() *board.Candidate { return nextFree(st, cands, sc).Pick })
	if res := nextFree(st, cands, sc); res.Pick == nil || !strings.Contains(res.Pick.Why(), "a claim not honoured: stale, desk "+deskOne) {
		t.Errorf("stale claim: pick %+v", res.Pick)
	}

	// released: no claim at all
	unserveClaim(t, one, claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	two.cfg.Board.Claims.StaleAfter.Duration = 24 * time.Hour
	cands = claimCands(claimIssue)
	two.skipClaimed(ctx, io.Discard, cands, func() *board.Candidate { return nextFree(st, cands, sc).Pick })
	if cands[0].Skip != "" || cands[0].Claim != "" {
		t.Errorf("released claim: %+v", cands[0])
	}

	// board.claims.off reads nothing
	two.cfg.Board.Claims.Off = true
	before := gh.Calls["list"] + gh.Calls["notModified"]
	two.skipClaimed(ctx, io.Discard, cands, func() *board.Candidate { return nextFree(st, cands, sc).Pick })
	if gh.Calls["list"]+gh.Calls["notModified"] != before {
		t.Errorf("claims off read GitHub: %v", gh.Calls)
	}

	// a read GitHub fails is a warning; the pick stands
	two.cfg.Board.Claims.Off = false
	claimGH = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("HTTP 403: rate limit") }
	var warn bytes.Buffer
	cands = claimCands(claimIssue)
	two.skipClaimed(ctx, &warn, cands, func() *board.Candidate { return nextFree(st, cands, sc).Pick })
	if res := nextFree(st, cands, sc); res.Pick == nil || !strings.Contains(warn.String(), "claims unread") {
		t.Errorf("failed read: pick %+v, warning %q", res.Pick, warn.String())
	}
	out := two.out.(*bytes.Buffer)
	out.Reset()
	if err := two.claimGate(ctx, "Worker two", claimIssue, false, false); err != nil || !strings.Contains(out.String(), "claims unread") {
		t.Errorf("failed read at the start: %v, %q", err, out.String())
	}
}

// agents start of a brief naming an issue another desk claims is refused
// with the claim's desk, session and time; --force starts over it, on record.
func TestClaimGateRefusesTheStartUnlessForced(t *testing.T) {
	one, two, _ := claimDesks(t)
	ctx := context.Background()
	p := serveClaim(t, one, "Worker one", claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	err := two.claimGate(ctx, "Worker two", claimIssue, false, false)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "desk "+deskOne) || !strings.Contains(err.Error(), p.Session) || !strings.Contains(err.Error(), " UTC") {
		t.Fatalf("claimGate = exit %d, %v", Code(err), err)
	}
	if err := two.claimGate(ctx, "Worker two", claimIssue, true, true); err != nil {
		t.Fatalf("dry run --force: %v", err)
	}
	if err := two.claimGate(ctx, "Worker two", claimIssue, true, false); err != nil {
		t.Fatalf("--force: %v", err)
	}
	evs, err := two.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Verb != verbStart || !strings.Contains(evs[0].Detail, "over the claim of desk "+deskOne) {
		t.Errorf("events = %+v, want the one --force start", evs)
	}
	if err := two.claimGate(ctx, "Worker two", claimIssue2, false, false); err != nil {
		t.Errorf("an unclaimed issue: %v", err)
	}
	if err := one.claimGate(ctx, "Worker one", claimIssue, false, false); err != nil {
		t.Errorf("the desk's own claim: %v", err)
	}
}

// Two desks claiming one item at once: the older comment wins, the other
// desk gives its claim back.
func TestPublishClaimedGivesBackALostRace(t *testing.T) {
	one, two, gh := claimDesks(t)
	ctx := context.Background()
	serveClaim(t, one, "Worker one", claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	me := serveClaim(t, two, "Worker two", claimIssue)
	var w bytes.Buffer
	err := two.publishClaimed(ctx, &w, me, claimIssue)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "claimed by desk "+deskOne) {
		t.Fatalf("publishClaimed = exit %d, %v (%s)", Code(err), err, w.String())
	}
	st, _ := two.store.Read()
	if len(st.Records) != 0 || len(st.Claims) != 0 {
		t.Errorf("the loser keeps %+v, %+v", st.Records, st.Claims)
	}
	var open []string
	for _, m := range markers(t, gh, claimIssue) {
		if m.Open() {
			open = append(open, m.Desk)
		}
	}
	if len(open) != 1 || open[0] != deskOne {
		t.Errorf("open claims = %v, want the winner's alone", open)
	}

	// the winner's own publish stands
	if err := one.publishClaimed(ctx, &w, state.Party{}, claimIssue); err != nil {
		t.Errorf("the winner: %v", err)
	}
}

func TestPlanClaims(t *testing.T) {
	const planIssue1, planIssue2 = "p/q#1", "p/q#2"
	now := time.Now()
	a := state.Party{Session: "a", Name: "A"}
	b := state.Party{Session: "b", Name: "B"}
	st := &state.State{
		Records: []state.Record{{Session: a, Issue: planIssue1}, {Session: b, Issue: planIssue2}, {Session: a, Issue: "not an issue"}, {Session: a, Issue: "p/q#4"}},
		Claims: []state.Claim{
			{Issue: "P/Q#1", Comment: 1, Session: a, Seen: now.Add(-time.Hour)},
			{Issue: planIssue2, Comment: 2, Session: a, Seen: now},
			{Issue: "p/q#3", Comment: 3, Session: b, Seen: now},
			{Issue: "p/q#4", Session: a, Seen: now.Add(-24 * time.Hour)}, // a pull request's
		},
	}
	publish, release := planClaims(st, now, 6*time.Hour)
	if len(publish) != 1 || publish[0].rec.Issue != planIssue2 || publish[0].prev == nil {
		t.Errorf("publish = %+v, want p/q#2's new session alone, never a pull request's", publish)
	}
	if len(release) != 1 || release[0].Issue != "p/q#3" {
		t.Errorf("release = %+v", release)
	}
}

// The review's race: the desk that loses owns a released comment older than
// the winner's. Its new claim is a new comment, newer than the winner's, so
// it loses; the winner, which read the thread before, keeps its claim.
func TestARaceIsLostByTheLaterClaimWhateverCommentsTheDeskOwns(t *testing.T) {
	one, two, gh := claimDesks(t)
	ctx := context.Background()
	var w bytes.Buffer
	// desk one held the issue earlier and released it
	serveClaim(t, one, "Worker one", claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	unserveClaim(t, one, claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	// desk two claims, and its check sees only the released comment
	meTwo := serveClaim(t, two, "Worker two", claimIssue)
	if err := two.publishClaimed(ctx, &w, meTwo, claimIssue); err != nil {
		t.Fatalf("the first claim: %v (%s)", err, w.String())
	}
	// desk one claims at the same time, its skip read before desk two's post
	meOne := serveClaim(t, one, "Worker three", claimIssue)
	err := one.publishClaimed(ctx, &w, meOne, claimIssue)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "claimed by desk "+deskTwo) {
		t.Fatalf("the later claim = exit %d, %v (%s)", Code(err), err, w.String())
	}
	if winner, err := two.lostClaim(ctx, claimIssue); err != nil || winner != "" {
		t.Errorf("the first claim now: %q, %v", winner, err)
	}
	var open []string
	for _, m := range markers(t, gh, claimIssue) {
		if m.Open() {
			open = append(open, m.Desk)
		}
	}
	if len(open) != 1 || open[0] != deskTwo {
		t.Errorf("open claims = %v, want desk two's alone", open)
	}
}

// --claim's publish waits for a sync another process runs, then publishes;
// one it cannot wait out is said, never taken for a published claim.
func TestPublishClaimedWaitsForARunningSync(t *testing.T) {
	one, _, gh := claimDesks(t)
	lock := flock.New(filepath.Join(one.store.Dir(), claimsLockFile))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v, %v", ok, err)
	}
	me := serveClaim(t, one, "Worker one", claimIssue)
	var w bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := one.publishClaimed(ctx, &w, me, claimIssue); err != nil || !strings.Contains(w.String(), "this desk's only") {
		t.Fatalf("with the lock held throughout: %v, %q", err, w.String())
	}
	if len(markers(t, gh, claimIssue)) != 0 {
		t.Fatal("published under another sync's lock")
	}

	w.Reset()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = lock.Unlock()
	}()
	if err := one.publishClaimed(context.Background(), &w, me, claimIssue); err != nil || w.Len() != 0 {
		t.Fatalf("after the other sync: %v, %q", err, w.String())
	}
	if ms := markers(t, gh, claimIssue); len(ms) != 1 || !ms[0].Open() {
		t.Errorf("markers = %+v", ms)
	}
}

// A record serving a pull request publishes no claim, and its end releases
// nothing on GitHub.
func TestAPullRequestCarriesNoClaim(t *testing.T) {
	one, _, gh := claimDesks(t)
	ctx := context.Background()
	gh.Pulls[claimIssue2] = true
	serveClaim(t, one, "Reviewer", claimIssue2)
	for range 2 {
		if err := one.syncClaims(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(gh.Comments(claimIssue2)) != 0 || gh.Calls["issue"] != 1 {
		t.Errorf("comments %v, calls %v", gh.Comments(claimIssue2), gh.Calls)
	}
	unserveClaim(t, one, claimIssue2)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := one.store.Read()
	if len(st.Claims) != 0 || gh.Calls["edit"] != 0 {
		t.Errorf("claims %+v, calls %v", st.Claims, gh.Calls)
	}
}

// sessions serve over another desk's claim says so.
func TestServeWarnsOverAnotherDesksClaim(t *testing.T) {
	one, two, _ := claimDesks(t)
	ctx := context.Background()
	serveClaim(t, one, "Worker one", claimIssue)
	if err := one.syncClaims(ctx); err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	two.warnClaimedElsewhere(ctx, &w, claimIssue)
	if !strings.Contains(w.String(), "claimed by desk "+deskOne) {
		t.Errorf("warning %q", w.String())
	}
	w.Reset()
	one.warnClaimedElsewhere(ctx, &w, claimIssue)
	if w.Len() != 0 {
		t.Errorf("own claim warned %q", w.String())
	}
}
