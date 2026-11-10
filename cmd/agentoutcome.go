package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const taskEndings = `finish with a merged pull request or --done --no-change "<reason and proof>", use --blocked --by <issue URL|#note>, or agents park on a wake`

// Only the worker owning a delivery may end it. Reviewers serving its PR
// report their review independently; that does not complete the issue.
func workerDeliveries(st *state.State, ag state.Agent) []int {
	var found []int
	for i, d := range st.Deliveries {
		served := slices.ContainsFunc(st.Records, func(r state.Record) bool {
			return r.Session.Is(ag.Party) && r.Ended.IsZero() && strings.EqualFold(r.Issue, d.Ref)
		})
		if served || d.Agent == ag.Name && !d.Started.Before(ag.AssignedAt) {
			found = append(found, i)
		}
	}
	return found
}

func (a *app) completionOutcome(ctx context.Context, st *state.State, ag state.Agent, done bool, noChange string, blocked bool, by string) ([]state.Event, error) {
	noChange, by = strings.TrimSpace(noChange), strings.TrimSpace(by)
	if blocked && done || noChange != "" && !done || by != "" && !blocked || blocked && by == "" {
		return nil, refused("choose one outcome: %s", taskEndings)
	}
	if !done && !blocked {
		if len(workerDeliveries(st, ag)) > 0 {
			return nil, refused("delivery task needs an outcome: %s", taskEndings)
		}
		return nil, nil
	}
	if !blocked && noChange == "" && slices.ContainsFunc(st.DeskReviews, func(r state.DeskReview) bool { return sameReviewSession(r.Reviewer, ag.Party) }) {
		return nil, nil
	}
	indices := workerDeliveries(st, ag)
	if blocked && len(indices) == 0 {
		return nil, refused("no owned delivery to block")
	}
	outcome, evidence := state.OutcomeMerged, ""
	switch {
	case blocked:
		outcome, evidence = state.OutcomeBlocked, by
		if err := a.blockDeliveries(ctx, st, indices, by); err != nil {
			return nil, err
		}
	case noChange != "":
		outcome, evidence = state.OutcomeNoChange, noChange
	default:
		for _, i := range indices {
			d := st.Deliveries[i]
			merged := false
			for _, ref := range d.Pulls {
				r, ok := parseRef(ref)
				if !ok {
					continue
				}
				reads, err := refStates(ctx, []github.PR{r})
				if err != nil {
					return nil, fmt.Errorf("verify completion of %s: %w", d.Ref, err)
				}
				if read := reads[r]; read.Kind == github.RefPullRequest && read.State == github.Merged {
					merged = true
					evidence = strings.TrimSpace(evidence + " " + deliveryURL(ref, true))
					break
				}
			}
			if !merged {
				return nil, refused("delivery %s has no merged pull request: %s", d.Ref, taskEndings)
			}
		}
	}
	return a.recordTaskOutcome(st, ag, outcome, evidence), nil
}

func (a *app) blockDeliveries(ctx context.Context, st *state.State, indices []int, by string) error {
	if id, note := parkNote(by); note {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == id })
		if i < 0 {
			return refused("blocker note #%d is not open", id)
		}
		for _, k := range indices {
			ref := st.Deliveries[k].Ref
			if st.Notes[i].HoldOf(ref) != nil {
				continue
			}
			mv, err := a.boardClient().Hold(ctx, ref)
			if err != nil {
				return err
			}
			h := state.ItemHold{Ref: mv.Ref, Status: mv.To, Was: mv.From}
			if st.Notes[i].Held == nil {
				st.Notes[i].Held = &h
			} else {
				st.Notes[i].AlsoHeld = append(st.Notes[i].AlsoHeld, h)
			}
		}
		return nil
	}
	if !strings.HasPrefix(by, "https://github.com/") {
		return refused("--by requires an issue URL or #note")
	}
	kind, err := resolveParkRef(ctx, by)
	if err != nil {
		return err
	}
	if kind != github.RefIssue {
		return refused("--by must name an open issue, not a pull request")
	}
	for _, i := range indices {
		if _, _, err := a.boardClient().MoveHeld(ctx, st.Deliveries[i].Ref, board.HeldStatus, by); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) recordTaskOutcome(st *state.State, ag state.Agent, outcome, evidence string) []state.Event {
	prev := slices.Clone(st.Deliveries)
	for _, i := range workerDeliveries(st, ag) {
		d := &st.Deliveries[i]
		d.Outcome, d.Evidence, d.OutcomeAt = outcome, evidence, a.now.UTC()
		switch outcome {
		case state.OutcomeNoChange:
			d.Step = state.DeliveryDone
		case state.OutcomeBlocked:
			d.Step = state.DeliveryBlocked
		case state.OutcomeStalled:
			d.Step = state.DeliveryStalled
		}
		d.Since = a.now.UTC()
	}
	return state.DeliveryEvents(prev, st.Deliveries, ag.Party)
}
