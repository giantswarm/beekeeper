// Package sim models the desk's flow of an issue from its start to its
// release as the supervisor loop is meant to run it, simulates that model,
// measures the real flow from the event log and compares the two: where
// the real system is slower than the model, what the log names as the
// cause, and which single change would gain the most released issues
// per day.
//
// The model is deliberately small. An issue is started by a worker when a
// slot is free; the worker pushes a pull request; CI runs; a reviewer is
// started once CI is green; the reviewer posts findings and asks
// for changes; the merge lane merges one pull request at a time; the
// release rolls out. The loop drives every transition but the worker's
// and the reviewer's own turns, so while the loop is down nothing starts,
// nothing is reviewed and nothing merges.
package sim

import (
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Stage names are the delivery steps of state.Delivery, so a model's
// stage compares with the step the loop records for a real delivery.
const (
	StageImplementing = state.DeliveryImplementing
	StageCI           = state.DeliveryCI
	StageReviewQueued = state.DeliveryReviewQueued
	StageReviewing    = state.DeliveryReviewing
	StageFixing       = state.DeliveryFixing
	StageMerging      = state.DeliveryMerging
	StageReleasing    = state.DeliveryReleasing
	// StageDecision is an issue parked on its person's answer, which the
	// delivery steps count inside implementing.
	StageDecision = "decision"
)

// Stages lists the steps in flow order.
var Stages = []string{StageImplementing, StageCI, StageReviewQueued, StageReviewing, StageFixing, StageMerging, StageReleasing}

// Stage is one step's service time: a lognormal distribution around
// Median with the spread Sigma (0 makes every sample the median).
type Stage struct {
	Median time.Duration `json:"median"`
	Sigma  float64       `json:"sigma"`
}

// Model is the flow with its capacities, service times and chances.
type Model struct {
	// Stages maps a stage name to its service time. review-queued is the
	// time the loop takes to start a reviewer once CI is green, merging the
	// gate's own run (devctl pr merge with its merge-result check), and
	// releasing the release workflow with the rollout.
	Stages map[string]Stage `json:"stages"`
	// Workers is the ceiling of busy agents (capacity.ceiling): workers,
	// fixers and reviewers share it; reviewers may exceed it by
	// ReviewHeadroom (capacity.reviewHeadroom).
	Workers        int `json:"workers"`
	ReviewHeadroom int `json:"reviewHeadroom"`
	// Lanes is how many repositories merge in parallel: one merge at a
	// time per lane.
	Lanes int `json:"lanes"`
	// LoopUptime is the share of time the supervisor loop runs, 0 to 1,
	// and LoopOutage the mean length of one down period.
	LoopUptime float64       `json:"loopUptime"`
	LoopOutage time.Duration `json:"loopOutage"`
	// RedCI is the chance a CI run is red and goes back to a fixer;
	// ChangesRequested the chance a review asks for changes instead of
	// fixing on the branch; Dropped the chance a started issue ends
	// without a pull request (research, closed unmerged, overlap).
	RedCI            float64 `json:"redCI"`
	ChangesRequested float64 `json:"changesRequested"`
	Dropped          float64 `json:"dropped"`
	// UpdateBranch adds a CI run to every merge, as devctl pr merge
	// --update-branch does when the base moved.
	UpdateBranch bool `json:"updateBranch"`
	// DecisionShare is the share of issues that wait for the person's
	// answer once, for the decision stage's service time.
	DecisionShare float64 `json:"decisionShare"`
	// Backlog is how many issues wait on the board at the start, and
	// ArrivalsPerDay how many are filed per day on top. A negative
	// backlog never runs out.
	Backlog        int     `json:"backlog"`
	ArrivalsPerDay float64 `json:"arrivalsPerDay"`
	// Horizon is the simulated time, Seed the random seed.
	Horizon time.Duration `json:"horizon"`
	Seed    int64         `json:"seed"`
}

// DefaultSigma is the spread of every default stage: the 90th percentile
// is about 2.8 times the median, as the real steps show.
const DefaultSigma = 0.8

// Default is the intended flow: the loop up, the stage times a worker, a
// reviewer, CI and the gate take when nothing waits, the chances the
// week's log shows for red CI and change requests, and no drops.
func Default() Model {
	return Model{
		Stages: map[string]Stage{
			StageImplementing: {Median: 30 * time.Minute, Sigma: DefaultSigma},
			StageCI:           {Median: 15 * time.Minute, Sigma: 0.3},
			StageReviewQueued: {Median: 2 * time.Minute, Sigma: 0.3},
			StageReviewing:    {Median: 10 * time.Minute, Sigma: DefaultSigma},
			StageFixing:       {Median: 20 * time.Minute, Sigma: DefaultSigma},
			StageMerging:      {Median: 5 * time.Minute, Sigma: 0.5},
			StageReleasing:    {Median: 20 * time.Minute, Sigma: 0.5},
			StageDecision:     {Median: 8 * time.Hour, Sigma: DefaultSigma},
		},
		Workers:          10,
		ReviewHeadroom:   2,
		Lanes:            3,
		LoopUptime:       1,
		LoopOutage:       time.Hour,
		RedCI:            0.2,
		ChangesRequested: 0.2,
		Dropped:          0,
		Backlog:          -1,
		Horizon:          7 * 24 * time.Hour,
		Seed:             1,
	}
}

// With returns m with the stage's median changed.
func (m Model) With(stage string, median time.Duration) Model {
	stages := make(map[string]Stage, len(m.Stages))
	for k, v := range m.Stages {
		stages[k] = v
	}
	s := stages[stage]
	s.Median = median
	if s.Sigma == 0 {
		s.Sigma = DefaultSigma
	}
	stages[stage] = s
	m.Stages = stages
	return m
}
