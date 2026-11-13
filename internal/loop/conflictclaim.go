package loop

import (
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
// request's conflict claim while it is on the roster, not done and not
// parked again since the claim. While it does, no other session is woken,
// started or continued to repair the same head.

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

// of reports whether name is the session c was claimed for or the one that
// holds it now.
func (c conflictClaim) of(name string) bool {
	return strings.EqualFold(c.Target, name) || strings.EqualFold(c.Holder, name)
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

// holder is the claim of ref's conflict at head that a live session took
// first.
func (cs conflictClaims) holder(ref, head string) (conflictClaim, bool) {
	var first conflictClaim
	found := false
	for _, c := range cs[strings.ToLower(ref)] {
		if SameCommit(c.Head, head) && (!found || c.At.Before(first.At)) {
			first, found = c, true
		}
	}
	return first, found
}

// claimHolder is the session on st's roster that holds a claim made for
// name at since: name itself, or the fresh continuation that took its task
// (followed through the starts' successors). "" when that session left the
// roster, is done, or parked again after since, which hands the pull request
// back to the loop.
func claimHolder(st *state.State, name string, since time.Time) string {
	seen := map[string]bool{}
	for name != "" && !seen[strings.ToLower(name)] {
		seen[strings.ToLower(name)] = true
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) }); i >= 0 {
			ag := st.Agents[i]
			if ag.Done || ag.Park != nil && ag.Park.At.After(since) {
				return ""
			}
			return ag.Name
		}
		next := ""
		for _, s := range slices.Concat(st.Starts, st.EndedWorkers) {
			if strings.EqualFold(s.Name, name) && s.Successor != nil {
				next = s.Successor.Name
			}
		}
		name = next
	}
	return ""
}

// conflictHeldWhy says why name gets no repair of url's conflict: c's
// session holds it.
func conflictHeldWhy(url, name string, c conflictClaim) string {
	return fmt.Sprintf("%s at head %s conflicts with its base; %s holds its conflict claim since %s, so %s gets no second repair",
		url, shortHead(c.Head), c.Holder, c.At.UTC().Format(time.RFC3339), name)
}

// holdConflictRepairs keeps one repair per conflicting head. A planned
// conflict repair for a session other than the live one that holds the
// head's conflict claim becomes one leave line per head, logged once: the
// parked session is neither woken, continued nor replaced by a fixer.
func holdConflictRepairs(actions []superloop.Action, held conflictClaims, taken []superloop.Taken) []superloop.Action {
	out := make([]superloop.Action, 0, len(actions))
	for _, act := range actions {
		c, ok := conflictHolderOf(act, held)
		if !ok {
			out = append(out, act)
			continue
		}
		key := fmt.Sprintf("leave %s %s@%s", act.Ref, conflictHeld, shortHead(act.Head))
		if slices.ContainsFunc(taken, func(t superloop.Taken) bool { return t.Key == key }) ||
			slices.ContainsFunc(out, func(a superloop.Action) bool { return a.Key == key }) {
			continue
		}
		out = append(out, superloop.Action{Key: key, Kind: superloop.KindLeave, Ref: act.Ref, URL: act.URL, Agent: act.Agent, Head: act.Head,
			Message: conflictHeldWhy(act.URL, act.Agent, c)})
	}
	return out
}

// conflictHolderOf is the claim that holds act's head when act is a
// conflict repair for another session than the claim's.
func conflictHolderOf(act superloop.Action, held conflictClaims) (conflictClaim, bool) {
	if act.Kind != superloop.KindRework || !strings.HasPrefix(act.Key, conflictKeyPrefix) || act.Head == "" {
		return conflictClaim{}, false
	}
	c, ok := held.holder(act.Ref, act.Head)
	if !ok || c.of(act.Agent) {
		return conflictClaim{}, false
	}
	return c, true
}

// shortHead is a head as the loop's lines name it.
func shortHead(sha string) string { return sha[:min(len(sha), 9)] }
