package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// claimGH is the gh CLI the claims are read and written through; a seam for
// the tests.
var claimGH github.GH = github.RunGH

// claimsParty books the claims' events.
var claimsParty = state.Party{Name: "beekeeper claims"}

const (
	claimsLockFile   = "claims.lock"
	claimsCacheFile  = "claims-cache.json"
	verbClaimPublish = "claim.publish"
	verbClaimRelease = "claim.release"
)

// claimsOn says whether the desk honours the other desks' claims:
// board.claims not off.
func (a *app) claimsOn() bool { return !a.cfg.Board.Claims.Off }

// claimDesk is the name the desk signs its claims with (board.claims.desk),
// "" when it publishes none: never the machine's name, which a public
// repository's issue would carry.
func (a *app) claimDesk() string {
	if !a.claimsOn() {
		return ""
	}
	return a.cfg.Board.Claims.Desk
}

// claimSession is how a claim names its session: the session's id, which
// the claiming desk resolves (sessions) and which says nothing of the work's
// internals, unlike a session's title.
func claimSession(p state.Party) string {
	return cmp.Or(p.Session, strings.TrimPrefix(p.HostSession, "local_"), "unknown")
}

func (a *app) claimClient() *claim.Client {
	return &claim.Client{GH: claimGH, Cache: filepath.Join(a.cfg.StateDir, claimsCacheFile)}
}

// claimable are the records the desk claims on GitHub, by lower-cased issue:
// a record of a session that runs (not Ended) and is no agent reporting
// idle, naming an issue (owner/repo#n); of several on one issue the newest.
func claimable(st *state.State) map[string]state.Record {
	out := map[string]state.Record{}
	for _, r := range st.Records {
		if !r.Ended.IsZero() || !issueRef.MatchString(r.Issue) || agentIdle(st, r.Session) {
			continue
		}
		k := strings.ToLower(r.Issue)
		if o, ok := out[k]; !ok || r.At.After(o.At) {
			out[k] = r
		}
	}
	return out
}

// claimWork is a claim to publish: the record, and the claim published for
// its issue before, if any.
type claimWork struct {
	rec  state.Record
	prev *state.Claim
}

// planClaims is what a sync does at now: publish each claimable record
// without a claim, with a claim naming another session, or one last
// refreshed refresh ago or longer; release each claim no record holds.
func planClaims(st *state.State, now time.Time, refresh time.Duration) (publish []claimWork, release []state.Claim) {
	want := claimable(st)
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range want {
			if !yield(k) {
				return
			}
		}
	}) {
		r := want[k]
		i := slices.IndexFunc(st.Claims, func(c state.Claim) bool { return strings.EqualFold(c.Issue, r.Issue) })
		if i < 0 {
			publish = append(publish, claimWork{rec: r})
			continue
		}
		c := st.Claims[i]
		if c.Comment == 0 {
			continue // a pull request: no claim
		}
		if !c.Session.Is(r.Session) || now.Sub(c.Seen) >= refresh {
			publish = append(publish, claimWork{rec: r, prev: &c})
		}
	}
	for _, c := range st.Claims {
		if _, ok := want[strings.ToLower(c.Issue)]; !ok {
			release = append(release, c)
		}
	}
	return publish, release
}

// claimsLockWait bounds how long a sync that must run (board next
// --claim's) waits for another to finish; claimsLockRetry is how often it
// tries the lock meanwhile.
const (
	claimsLockWait  = time.Minute
	claimsLockRetry = 200 * time.Millisecond
)

// syncClaims brings the desk's marker comments in line with its records
// (planClaims). One sync runs at a time on the state; while another runs,
// this one returns at once, the running one or the next doing its work.
func (a *app) syncClaims(ctx context.Context) error {
	if a.claimDesk() == "" {
		return nil
	}
	lock := flock.New(filepath.Join(a.store.Dir(), claimsLockFile))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	return a.syncClaimsLocked(ctx)
}

// syncClaimsNow is syncClaims that waits for a running sync to finish, up
// to claimsLockWait, instead of leaving its work to it: a claim that must be
// on GitHub before its caller goes on.
func (a *app) syncClaimsNow(ctx context.Context) error {
	if a.claimDesk() == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, claimsLockWait)
	defer cancel()
	lock := flock.New(filepath.Join(a.store.Dir(), claimsLockFile))
	ok, err := lock.TryLockContext(ctx, claimsLockRetry)
	switch {
	case err != nil:
		return fmt.Errorf("waiting for the claims' sync: %w", err)
	case !ok:
		return errors.New("waiting for the claims' sync: the lock stayed held")
	}
	defer func() { _ = lock.Unlock() }()
	return a.syncClaimsLocked(ctx)
}

// syncClaimsLocked is one sync under the claims' lock: one comment per
// issue and desk, a claim that holds edited in place, a claim that ended
// released. GitHub's answers are written back to the state; an error names
// each claim that failed, the rest stand.
func (a *app) syncClaimsLocked(ctx context.Context) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	publish, release := planClaims(st, now, a.cfg.Board.Claims.Refresh.Duration)
	if len(publish) == 0 && len(release) == 0 {
		return nil
	}
	desk, cl := a.claimDesk(), a.claimClient()
	var done []state.Claim
	var gone []string
	var errs []error
	for _, w := range publish {
		c, err := publishClaim(ctx, cl, desk, w, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, c)
	}
	for _, c := range release {
		if c.Comment == 0 {
			gone = append(gone, c.Issue)
			continue
		}
		m := claim.Marker{Desk: desk, Session: claimSession(c.Session), At: c.At, Seen: c.Seen, Released: now}
		if err := cl.Edit(ctx, c.Issue, c.Comment, m); err != nil && !errors.Is(err, claim.ErrGone) {
			errs = append(errs, err)
			continue
		}
		gone = append(gone, c.Issue)
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		for _, c := range done {
			st.Claims = slices.DeleteFunc(st.Claims, func(o state.Claim) bool { return strings.EqualFold(o.Issue, c.Issue) })
			st.Claims = append(st.Claims, c)
			if c.Comment != 0 && c.Seen.Equal(c.At) {
				evs = append(evs, about(event(claimsParty, verbClaimPublish, "%s for %s (desk %s, comment %d)", c.Issue, c.Session.Name, desk, c.Comment), c.Issue))
			}
		}
		for _, issue := range gone {
			i := slices.IndexFunc(st.Claims, func(o state.Claim) bool { return strings.EqualFold(o.Issue, issue) })
			if i >= 0 && st.Claims[i].Comment != 0 {
				evs = append(evs, about(event(claimsParty, verbClaimRelease, "%s (desk %s)", issue, desk), issue))
			}
			st.Claims = slices.DeleteFunc(st.Claims, func(o state.Claim) bool { return strings.EqualFold(o.Issue, issue) })
		}
		return evs, nil
	})
	return errors.Join(append(errs, err)...)
}

// publishClaim writes w's claim on its issue. A claim that holds (the one
// published before, or the desk's open comment the thread has) is edited in
// place and keeps its comment, so its place in a race; a new claim is a new
// comment, so comment ids follow the order claims open in (lostClaim), and
// the desk's released comments on the thread are deleted: one comment per
// issue and desk. A pull request carries no claim (Comment 0).
func publishClaim(ctx context.Context, cl *claim.Client, desk string, w claimWork, now time.Time) (state.Claim, error) {
	at, id := now, int64(0)
	if w.prev != nil {
		id = w.prev.Comment
		if w.prev.Session.Is(w.rec.Session) {
			at = w.prev.At
		}
	}
	m := claim.Marker{Desk: desk, Session: claimSession(w.rec.Session), At: at, Seen: now}
	if id != 0 {
		err := cl.Edit(ctx, w.rec.Issue, id, m)
		switch {
		case err == nil:
			return state.Claim{Issue: w.rec.Issue, Comment: id, Session: w.rec.Session, At: at, Seen: now}, nil
		case !errors.Is(err, claim.ErrGone):
			return state.Claim{}, err
		}
		id = 0 // deleted on GitHub: claimed anew
	}
	pull, err := cl.Pull(ctx, w.rec.Issue)
	if err != nil {
		return state.Claim{}, err
	}
	if pull {
		return state.Claim{Issue: w.rec.Issue, Session: w.rec.Session, At: at, Seen: now}, nil
	}
	cs, err := cl.Read(ctx, w.rec.Issue)
	if err != nil {
		return state.Claim{}, err
	}
	var released []int64
	for _, c := range cs {
		if !strings.EqualFold(c.Desk, desk) {
			continue
		}
		if c.Open() && id == 0 { // the desk's claim, which the state lost
			if err := cl.Edit(ctx, w.rec.Issue, c.ID, m); err == nil {
				id = c.ID
				continue
			} else if !errors.Is(err, claim.ErrGone) {
				return state.Claim{}, err
			}
		}
		if c.ID != id {
			released = append(released, c.ID)
		}
	}
	if id == 0 {
		if id, err = cl.Post(ctx, w.rec.Issue, m); err != nil {
			return state.Claim{}, err
		}
	}
	for _, old := range released {
		// a released comment left behind holds nothing: the claim stands
		_ = cl.Delete(ctx, w.rec.Issue, old)
	}
	return state.Claim{Issue: w.rec.Issue, Comment: id, Session: w.rec.Session, At: at, Seen: now}, nil
}

// warnClaims syncs the claims and says a failure on w: the local record
// stands whatever GitHub answered.
func (a *app) warnClaims(ctx context.Context, w io.Writer) {
	if err := a.syncClaims(ctx); err != nil {
		_, _ = fmt.Fprintf(w, "beekeeper: claims on GitHub: %v\n", err)
	}
}

// otherClaims reads issue's claims of other desks: the live ones, oldest
// first, and the stale ones.
func (a *app) otherClaims(ctx context.Context, issue string) (live, stale []claim.Comment, err error) {
	cs, err := a.claimClient().Read(ctx, issue)
	if err != nil {
		return nil, nil, err
	}
	live, stale = claim.Others(cs, a.claimDesk(), time.Now(), a.cfg.Board.Claims.StaleAfter.Duration)
	return live, stale, nil
}

// claimedPrefix opens the skip reason of an item another desk claims.
const claimedPrefix = "claimed by desk "

// skipClaimed walks the picks of pick in order and marks each one another
// desk claims skipped ("claimed by desk <name> (session …) since …") until
// one is not, which a stale claim of another desk only annotates
// (Candidate.Claim). Only the candidates up to that pick are read. A read
// GitHub fails ends the walk with a warning on warn: the pick stands as if
// no other desk claimed it.
func (a *app) skipClaimed(ctx context.Context, warn io.Writer, cands []board.Candidate, pick func() *board.Candidate) {
	if !a.claimsOn() {
		return
	}
	staleAfter := a.cfg.Board.Claims.StaleAfter.Duration
	for {
		p := pick()
		if p == nil || !issueRef.MatchString(p.Ref) {
			return
		}
		i := slices.IndexFunc(cands, func(c board.Candidate) bool { return strings.EqualFold(c.Ref, p.Ref) && c.Skip == "" })
		if i < 0 {
			return
		}
		live, stale, err := a.otherClaims(ctx, p.Ref)
		if err != nil {
			_, _ = fmt.Fprintf(warn, "beekeeper: claims unread, %s and the items after it picked as if no other desk claimed them: %v\n", p.Ref, err)
			return
		}
		if len(live) == 0 {
			if len(stale) > 0 {
				cands[i].Claim = "not honoured: " + stale[0].Says(time.Now(), staleAfter)
			}
			return
		}
		cands[i].Skip = "claimed by " + live[0].Says(time.Now(), staleAfter)
	}
}

// lostClaim checks, after me claimed issue and the desk published it, that
// no other desk's live claim on it is older than the desk's own: the lower
// comment id wins, a new claim being a new comment (publishClaim). It
// returns the winner's line, "" when the desk's claim stands, and an error
// when the thread holds no open claim of the desk.
func (a *app) lostClaim(ctx context.Context, issue string) (string, error) {
	cs, err := a.claimClient().Read(ctx, issue)
	if err != nil {
		return "", err
	}
	staleAfter := a.cfg.Board.Claims.StaleAfter.Duration
	live, _ := claim.Others(cs, a.claimDesk(), time.Now(), staleAfter)
	var mine *claim.Comment
	for i := range cs {
		if strings.EqualFold(cs[i].Desk, a.claimDesk()) && cs[i].Open() {
			mine = &cs[i]
		}
	}
	switch {
	case mine == nil:
		return "", fmt.Errorf("%s holds no open claim of desk %s", issue, a.claimDesk())
	case len(live) == 0 || live[0].ID > mine.ID:
		return "", nil
	}
	return live[0].Says(time.Now(), staleAfter), nil
}

// claimGate refuses the start of name, whose brief serves issue, while
// another desk's live claim holds it; force starts it anyway, which the
// event log records (agents.start), and a dry run only says so. A stale
// claim is said, never a refusal, and so is a read GitHub fails.
func (a *app) claimGate(ctx context.Context, name, issue string, force, dryRun bool) error {
	if issue == "" || !a.claimsOn() {
		return nil
	}
	live, stale, err := a.otherClaims(ctx, issue)
	if err != nil {
		_, err = fmt.Fprintf(a.out, "claims unread, started as if no other desk claimed %s: %v\n", issue, err)
		return err
	}
	staleAfter := a.cfg.Board.Claims.StaleAfter.Duration
	if len(live) == 0 {
		if len(stale) > 0 {
			_, err = fmt.Fprintf(a.out, "%s carries a claim not honoured: %s\n", issue, stale[0].Says(time.Now(), staleAfter))
		}
		return err
	}
	says := live[0].Says(time.Now(), staleAfter)
	switch {
	case !force:
		return refused("%s is claimed by %s: another desk works it; --force starts anyway", issue, says)
	case dryRun:
		_, err = fmt.Fprintf(a.out, "dry run over the claim of %s on %s (--force)\n", says, issue)
		return err
	}
	by, err := a.caller()
	if err != nil {
		return err
	}
	if err := a.store.Record(about(event(by, verbStart, "%s: started over the claim of %s on %s (--force)", name, says, issue), issue)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "started over the claim of %s on %s (--force)\n", says, issue)
	return err
}

// publishClaimed publishes the claim me just recorded on issue (board next
// --claim) and, should another desk's claim on it be older, gives it back:
// the record removed, the claim released, refused with the winner. A claim
// GitHub did not take stays local, said on w.
func (a *app) publishClaimed(ctx context.Context, w io.Writer, me state.Party, issue string) error {
	if !a.claimsOn() {
		return nil
	}
	if a.claimDesk() == "" {
		_, err := fmt.Fprintln(w, "beekeeper: board.claims.desk unset: the claim is this desk's only, no other desk sees it")
		return err
	}
	if err := a.syncClaimsNow(ctx); err != nil {
		_, err = fmt.Fprintf(w, "beekeeper: claims on GitHub: %v; the claim is this desk's only until a sync publishes it\n", err)
		return err
	}
	winner, err := a.lostClaim(ctx, issue)
	if err != nil || winner == "" {
		if err != nil {
			_, err = fmt.Fprintf(w, "beekeeper: claims on GitHub: %v; the claim is this desk's only until a sync publishes it\n", err)
		}
		return err
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records = slices.DeleteFunc(st.Records, func(r state.Record) bool { return r.Session.Is(me) && strings.EqualFold(r.Issue, issue) })
		return []state.Event{about(event(me, "session.unserve", "%s: given back, claimed by %s first", issue, winner), issue)}, nil
	})
	if err != nil {
		return err
	}
	a.warnClaims(ctx, w)
	return refused("%s was claimed by %s first: the claim is given back; board next --claim picks again", issue, winner)
}

// claims is a watch's sync of the desk's claims: a failure is a condition
// of its own, said once until a sync succeeds.
func (w *watcher) claims(ctx context.Context) {
	if err := w.syncClaims(ctx); err != nil {
		w.emit("claims", "CLAIMS on GitHub failed: %v", err)
		return
	}
	w.clear("claims")
}

// warnClaimedElsewhere says on w when another desk's live claim holds issue,
// which sessions serve records over: two desks then work it.
func (a *app) warnClaimedElsewhere(ctx context.Context, w io.Writer, issue string) {
	if !a.claimsOn() || !issueRef.MatchString(issue) {
		return
	}
	live, _, err := a.otherClaims(ctx, issue)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(w, "beekeeper: claims unread for %s: %v\n", issue, err)
	case len(live) > 0:
		_, _ = fmt.Fprintf(w, "beekeeper: %s is claimed by %s as well: two desks work it now\n", issue, live[0].Says(time.Now(), a.cfg.Board.Claims.StaleAfter.Duration))
	}
}
