package loop

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// A conflict repair is the loop's wake or start that tells one session to
// resolve a pull request's conflict with its base at one head: the action
// keyed "conflict <ref>@<head> <parked>". The session it was claimed for,
// or the fresh continuation that took that session's task, holds the pull
// request's conflict claim while it is on the roster, busy with a task, not
// done and not parked again since the claim. While it does, no other session
// is woken, started or continued for that pull request.

// conflictKeyPrefix starts the key of a conflict repair.
const conflictKeyPrefix = "conflict "

// conflictHeld is the reason of the leave line for a conflict repair the
// loop does not give a second session.
const conflictHeld = "conflict-held"

// conflictClaim is one conflict repair's claim: its key, the head it
// repairs, when it was claimed, the session it was claimed for (Target) and
// the session on the roster that holds it now (Holder).
type conflictClaim struct {
	Key, Head, Target, Holder string
	At                        time.Time
}

// conflictClaims are the conflict claims live sessions hold, by pull request
// (lower-case owner/repo#n).
type conflictClaims map[string][]conflictClaim

// heldConflicts reads the conflict claims in claims, the loop's claims as
// its history folds them (taken ones included), and keeps those a live
// session of st holds.
func heldConflicts(st *state.State, claims map[string]LoopClaim) conflictClaims {
	out := conflictClaims{}
	for key, c := range claims {
		rest, ok := strings.CutPrefix(key, conflictKeyPrefix)
		if !ok {
			continue
		}
		target, _, _ := strings.Cut(rest, " ")
		ref, head, ok := strings.Cut(target, "@")
		if !ok || ref == "" || head == "" {
			continue
		}
		holder := claimHolder(st, c.Target, c.At)
		if holder == "" {
			continue
		}
		k := strings.ToLower(ref)
		out[k] = append(out[k], conflictClaim{Key: key, Head: head, Target: c.Target, Holder: holder, At: c.At})
	}
	return out
}

// forPull is the live conflict claim holding ref: the earliest one matching
// head when head is non-empty and any matches, else the earliest live claim
// on ref.
func (cs conflictClaims) forPull(ref, head string) (conflictClaim, bool) {
	claims := cs[strings.ToLower(ref)]
	if len(claims) == 0 {
		return conflictClaim{}, false
	}
	var first conflictClaim
	found := false
	if head != "" {
		for _, c := range claims {
			if SameCommit(c.Head, head) && (!found || c.At.Before(first.At)) {
				first, found = c, true
			}
		}
		if found {
			return first, true
		}
	}
	for _, c := range claims {
		if !found || c.At.Before(first.At) {
			first, found = c, true
		}
	}
	return first, found
}

// claimHolder is the session on st's roster that holds a claim made for
// name at since: name itself, or the fresh continuation that took its task
// (followed through the starts' successors). "" when that session left the
// roster, is idle or done, or parked again after since, which hands the pull
// request back to the loop.
func claimHolder(st *state.State, name string, since time.Time) string {
	seen := map[string]bool{}
	for step := 0; name != "" && !seen[strings.ToLower(name)]; step++ {
		seen[strings.ToLower(name)] = true
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) }); i >= 0 {
			ag := st.Agents[i]
			if ag.Done || ag.Task == "" {
				return ""
			}
			if ag.Park != nil && (ag.Park.At.After(since) || step > 0 && len(ag.Park.Continuation) == 0) {
				return ""
			}
			return ag.Name
		}
		var latest *state.Start
		for _, s := range slices.Concat(st.Starts, st.EndedWorkers) {
			if !strings.EqualFold(s.Name, name) {
				continue
			}
			if latest == nil || s.At.After(latest.At) || s.At.Equal(latest.At) && (s.Successor != nil || latest.Successor == nil) {
				cp := s
				latest = &cp
			}
		}
		if latest == nil || latest.Successor == nil {
			return ""
		}
		name = latest.Successor.Name
	}
	return ""
}

// conflictHeldWhy says why name gets no repair of url's conflict: c's
// session holds it.
func conflictHeldWhy(url, name string, c conflictClaim) string {
	return fmt.Sprintf("%s at head %s conflicts with its base; %s holds its conflict claim since %s, so %s gets no second repair",
		url, shortHead(c.Head), c.Holder, c.At.UTC().Format(time.RFC3339), name)
}

// holdConflictRepairs keeps one session on a pull request whose conflict
// claim a live session holds. A planned wake or fixer start for another
// session on that pull request becomes one leave line per head, logged once:
// the other session is neither woken, continued nor replaced by a fixer.
func holdConflictRepairs(actions []superloop.Action, held conflictClaims, taken []superloop.Taken) []superloop.Action {
	out := make([]superloop.Action, 0, len(actions))
	for _, act := range actions {
		c, ok := conflictHolderOf(act, held)
		if !ok {
			out = append(out, act)
			continue
		}
		head := cmp.Or(act.Head, c.Head)
		key := fmt.Sprintf("leave %s %s@%s", act.Ref, conflictHeld, shortHead(head))
		if slices.ContainsFunc(taken, func(t superloop.Taken) bool { return t.Key == key }) ||
			slices.ContainsFunc(out, func(a superloop.Action) bool { return a.Key == key }) {
			continue
		}
		out = append(out, superloop.Action{Key: key, Kind: superloop.KindLeave, Ref: act.Ref, URL: act.URL, Agent: act.Agent, Head: head,
			Message: conflictHeldWhy(act.URL, act.Agent, c)})
	}
	return out
}

// conflictHolderOf is the claim that holds act's pull request when act would
// wake or start another session on it.
func conflictHolderOf(act superloop.Action, held conflictClaims) (conflictClaim, bool) {
	switch act.Kind {
	case superloop.KindRework, superloop.KindOwner, superloop.KindRed, superloop.KindForward:
	default:
		return conflictClaim{}, false
	}
	c, ok := held.forPull(act.Ref, act.Head)
	if !ok || act.Key == c.Key {
		return conflictClaim{}, false
	}
	if (act.Kind == superloop.KindForward || act.Kind == superloop.KindRed) && strings.EqualFold(c.Holder, act.Agent) {
		return conflictClaim{}, false
	}
	return c, true
}

// shortHead is a head as the loop's lines name it.
func shortHead(sha string) string { return sha[:min(len(sha), 9)] }
