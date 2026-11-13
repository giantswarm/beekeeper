package config

import (
	"fmt"
	"slices"
	"strings"
)

// The events the sink may dispatch: those that carry the issue or pull
// request they concern (owner/repo#n) as their subject.
const (
	// SinkPlanGrilling is a note for the guide's person admitted on a plan
	// pull request at its grill stage.
	SinkPlanGrilling = "plan.grilling"
	// SinkPlanReady is a note for the guide's person admitted on an open
	// plan pull request whose stage check is green: the plan awaits review.
	SinkPlanReady = "plan.ready"
	// SinkServe is a session's record of the issue it serves (sessions
	// serve), SinkBoardClaim one board next --claim records.
	SinkServe      = "session.serve"
	SinkBoardClaim = "board.claim"
	// SinkMerged is a pull request the merge gate merged, with its release.
	SinkMerged = "merged"
)

// SinkVerbs are the events the sink may dispatch, the default of
// sink.verbs.
var SinkVerbs = []string{SinkPlanGrilling, SinkPlanReady, SinkServe, SinkBoardClaim, SinkMerged}

// Sink dispatches Workflow of Repo (workflow_dispatch) for every event of
// Verbs, within a watch.interval of the event, with the inputs verb,
// subject (owner/repo#n) and time (RFC 3339, UTC), nothing else. Unset
// (no repo), nothing is dispatched.
type Sink struct {
	// Repo is the workflow's repository, owner/repo.
	Repo string `yaml:"repo"`
	// Workflow is the workflow's file name or id, refresh.yaml.
	Workflow string `yaml:"workflow"`
	// Ref is the branch or tag the workflow runs on (default: the
	// repository's default branch).
	Ref string `yaml:"ref"`
	// Verbs are the events dispatched, of SinkVerbs (default: all of them).
	Verbs []string `yaml:"verbs"`
}

// Enabled reports whether a workflow is configured.
func (s Sink) Enabled() bool { return s.Repo != "" }

// Selects reports whether an event of verb is dispatched.
func (s Sink) Selects(verb string) bool { return s.Enabled() && slices.Contains(s.Verbs, verb) }

func (s *Sink) defaults() {
	if s.Enabled() && len(s.Verbs) == 0 {
		s.Verbs = slices.Clone(SinkVerbs)
	}
}

func (s Sink) validateEach(report func(error)) {
	if !s.Enabled() {
		if s.Workflow != "" || s.Ref != "" || len(s.Verbs) > 0 {
			report(fmt.Errorf("sink: no repo: set sink.repo, the workflow's owner/repo, or nothing of sink"))
		}
		return
	}
	ownerReposEach("sink.repo", []string{s.Repo}, report)
	if strings.TrimSpace(s.Workflow) == "" {
		report(fmt.Errorf("sink.workflow: the workflow of %s to dispatch, its file name or id", s.Repo))
	}
	for _, v := range s.Verbs {
		if !slices.Contains(SinkVerbs, v) {
			report(fmt.Errorf("sink.verbs: %q is none of %s", v, strings.Join(SinkVerbs, ", ")))
		}
	}
}
