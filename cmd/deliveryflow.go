package cmd

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	deliveryFlowing = "flowing"
	deliveryPerson  = "waiting-on-person"
	deliveryBlocked = "blocked"
)

// External events cannot be turned into decisions merely by parking on a
// person. Unknown prose stays in WIP: only positive evidence frees a place.
var externalDeliveryWait = regexp.MustCompile(`(?i)\b(?:physical visit|technician|site visit|external party|on.?site measurement|physical measurement)\b`)

func deliveryMatches(d state.Delivery, ref string) bool {
	if ref == "" {
		return false
	}
	if r, ok := parseRef(ref); ok {
		ref = refName(r)
	}
	return strings.EqualFold(d.Ref, ref) || slices.ContainsFunc(d.Pulls, func(p string) bool { return strings.EqualFold(p, ref) })
}

func deliveryAgent(st *state.State, d state.Delivery, ag state.Agent) bool {
	return !ag.Done && ag.Task != "" && (d.Agent != "" && strings.EqualFold(d.Agent, ag.Name) || deliveryMatches(d, loop.LoopServes(st, ag)) || ag.Park != nil && deliveryMatches(d, loop.LoopParkOn(ag.Park)))
}

func classifyDelivery(st *state.State, d state.Delivery, now time.Time, p deliveryPolicy) deliveryFlow {
	return deliveryFlowSeen(st, d, now, p, map[string]bool{})
}

func deliveryFlowSeen(st *state.State, d state.Delivery, now time.Time, p deliveryPolicy, seen map[string]bool) deliveryFlow {
	flowing := deliveryFlow{Class: deliveryFlowing}
	key := strings.ToLower(d.Ref)
	// An unresolved cycle must not open admission.
	if seen[key] {
		return flowing
	}
	seen[key] = true
	defer delete(seen, key)
	// A reviewer or fixer can still progress a delivery whose original worker
	// waits on input. Running work takes precedence over a parked participant.
	for _, ag := range st.Agents {
		if deliveryAgent(st, d, ag) && (ag.Park == nil || !ag.Park.Resumable.IsZero()) {
			return flowing
		}
	}
	for _, n := range st.Notes {
		if !guides(p.person, &n) {
			continue
		}
		linked := slices.ContainsFunc(n.Refs, func(ref string) bool { return deliveryMatches(d, ref) })
		for _, ag := range st.Agents {
			if deliveryAgent(st, d, ag) && ag.Park != nil {
				id, ok := parkNote(ag.Park.On)
				linked = linked || ok && id == n.ID
			}
		}
		if linked {
			return deliveryFlow{Class: deliveryPerson, Reason: "open decision note"}
		}
	}
	for _, ag := range st.Agents {
		if !deliveryAgent(st, d, ag) || ag.Park == nil {
			continue
		}
		park := ag.Park
		if park.External {
			return deliveryFlow{Class: deliveryBlocked, Reason: park.Waits}
		}
		for _, t := range st.Timers {
			// Workers arm their wake timer before parking. Match a pending
			// occurrence, not its creation time; exclude timers bound to an
			// older park. A due probe can still be waiting on its condition.
			if strings.EqualFold(t.Wake, ag.Name) && t.Fired.IsZero() && (t.WakeParkAt.IsZero() || t.WakeParkAt.Equal(park.At)) {
				if now.Sub(park.At) < 24*time.Hour && t.Due.Sub(park.At) < 24*time.Hour && (t.Until.IsZero() || t.Until.Sub(park.At) < 24*time.Hour) {
					return flowing
				}
				return deliveryFlow{Class: deliveryBlocked, Reason: "timer waits at least a day"}
			}
		}
		if externalDeliveryWait.MatchString(park.Waits) {
			return deliveryFlow{Class: deliveryBlocked, Reason: park.Waits}
		}
		on := loop.LoopParkOn(park)
		if !deliveryMatches(d, on) {
			for _, other := range st.Deliveries {
				if deliveryMatches(other, on) && other.Active() && (!deliveryOwned(st, other, now, p) || deliveryFlowSeen(st, other, now, p, seen).Class != deliveryFlowing) {
					return deliveryFlow{Class: deliveryBlocked, Reason: "dependency is not flowing: " + deliveryURL(other.Ref, false)}
				}
			}
		}
		if flow, ok := deliveryPersonWait(st, on, park.Waits, p.person); ok {
			return flow
		}
	}
	return flowing
}

// deliveryPersonWords are what only a person gives. A wait's other words are
// no evidence of one: a worker keeps its continuation in an agents note,
// finishes a release note or asks for the loop's review.
var deliveryPersonWords = regexp.MustCompile(`(?i)\b(?:persons?|decisions?|answers?|approvals?|repl(?:y|ies))\b`)

// deliveryPersonWait classifies a park that waits on a person. A note covers
// its wait: an open one is in its addressee's queue, and a closed one was
// answered or closed, so the watch resumes the park. Any other target (an
// issue or pull request, a hold, a lease, an alert or a release) settles the
// park, whatever its words say. Only an untargeted wait that names the person,
// or what only a person gives, is missing its note.
func deliveryPersonWait(st *state.State, on, waits, person string) (deliveryFlow, bool) {
	if id, ok := parkNote(on); ok {
		open := slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == id })
		return deliveryFlow{Class: deliveryPerson, Reason: waits}, open
	}
	if _, _, ok := parkCentralNote(on); ok {
		return deliveryFlow{Class: deliveryPerson, Reason: waits}, true
	}
	if on != "" || !deliveryPersonWords.MatchString(waits) && !namesPerson(waits, person) {
		return deliveryFlow{}, false
	}
	return deliveryFlow{Class: deliveryPerson, Reason: waits, MissingNote: true}, true
}
