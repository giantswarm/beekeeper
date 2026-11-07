package state

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Delivery steps are observations, never start or capacity admission rules.
const (
	DeliveryUnknown       = "unknown"
	DeliveryImplementing  = "implementing"
	DeliveryCI            = "ci"
	DeliveryReviewQueued  = "review-queued"
	DeliveryReviewing     = "reviewing"
	DeliveryFixing        = "fixing"
	DeliveryMerging       = "merging"
	DeliveryReleasing     = "releasing"
	DeliveryDone          = "done"
	DeliveryDropped       = "dropped"
	DeliveryStepVerb      = "delivery.step"
	DeliveryNoRelease     = "no_release"
	DeliveryReleaseFailed = "ci_failed"
)

// Delivery is one issue (or an issue-less desk pull request) from start to release.
// Ref and Pulls use the same canonical references as serve records.
type Delivery struct {
	Ref            string    `json:"ref"`
	Pulls          []string  `json:"pulls"`
	Step           string    `json:"step"`
	Since          time.Time `json:"since"`
	Started        time.Time `json:"started"`
	Agent          string    `json:"agent,omitempty"`
	Waits          string    `json:"waits,omitempty"`
	Release        string    `json:"release,omitempty"`
	ReleaseChecked time.Time `json:"releaseChecked,omitempty"`
	Verdict        string    `json:"verdict,omitempty"`
}

// Active includes unknown observations: an unreadable pull never frees its place.
func (d Delivery) Active() bool { return d.Step != DeliveryDone && d.Step != DeliveryDropped }

// DeliveryEvents compares against the last committed observation, not a pass's
// earlier snapshot. Store.Update commits these events with the new reading.
func DeliveryEvents(prev, next []Delivery, by Party) []Event {
	var events []Event
	for _, d := range next {
		i := slices.IndexFunc(prev, func(p Delivery) bool { return strings.EqualFold(p.Ref, d.Ref) })
		if i >= 0 && prev[i].Step == d.Step && prev[i].Started.Equal(d.Started) {
			continue
		}
		raw, _ := json.Marshal(d)
		events = append(events, Event{At: d.Since, By: by, Verb: DeliveryStepVerb, Subject: d.Ref, Detail: string(raw)})
	}
	return events
}

// StartDelivery runs under the caller's serve-record lock. A fixer or a second
// worker serving the same active issue does not reset its delivery clock.
func (s *State) StartDelivery(ref, agent, waits string, at time.Time) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return
	}
	i := slices.IndexFunc(s.Deliveries, func(d Delivery) bool { return strings.EqualFold(d.Ref, ref) })
	if i >= 0 && s.Deliveries[i].Active() {
		return
	}
	d := Delivery{Ref: ref, Pulls: []string{}, Step: DeliveryImplementing, Since: at, Started: at, Agent: agent, Waits: waits}
	if i < 0 {
		s.Deliveries = append(s.Deliveries, d)
	} else {
		s.Deliveries[i] = d
	}
}
