// Package replay re-runs every recorded decision's rule from its recorded inputs and counts the
// decisions whose rule now answers differently (plan §3). The `replay` subcommand and the scenario
// tool of the same name call it.
package replay

import (
	"context"
	"fmt"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
	"github.com/giantswarm/beekeeper/internal/history"
)

// Mismatch is a decision whose rule answers differently on its recorded inputs.
type Mismatch struct {
	ID       int64
	Point    string
	Subject  string
	Recorded string
	Replayed string
	Err      error
}

// Result is what a replay found.
type Result struct {
	Decisions  int
	Mismatches []Mismatch
}

// Option configures a replay.
type Option func(*options)

type options struct {
	spec *v1alpha1.InstallationSpec
	rule func(spec *v1alpha1.InstallationSpec, point string, inputs map[string]any) (string, error)
	page func(Mismatch)
}

// WithSpec replays with an Installation's configuration (the ready? patterns); the defaults otherwise.
func WithSpec(spec *v1alpha1.InstallationSpec) Option { return func(o *options) { o.spec = spec } }

// WithRule replaces the rules (tests).
func WithRule(f func(spec *v1alpha1.InstallationSpec, point string, inputs map[string]any) (string, error)) Option {
	return func(o *options) { o.rule = f }
}

// OnMismatch is called for every mismatch as it is found.
func OnMismatch(f func(Mismatch)) Option { return func(o *options) { o.page = f } }

// Run replays every recorded decision and returns how many there were and how many mismatched.
func Run(ctx context.Context, r history.Reader, opts ...Option) (decisions, mismatches int, err error) {
	res, err := Replay(ctx, r, opts...)
	return res.Decisions, len(res.Mismatches), err
}

// Replay replays every recorded decision, oldest first, and lists the mismatches.
func Replay(ctx context.Context, r history.Reader, opts ...Option) (Result, error) {
	o := options{rule: decision.Replay}
	for _, opt := range opts {
		opt(&o)
	}
	var res Result
	after := int64(0)
	for {
		rows, err := r.Decisions(ctx, history.DecisionFilter{AfterID: after})
		if err != nil {
			return res, fmt.Errorf("read decisions after %d: %w", after, err)
		}
		progressed := false
		for _, d := range rows {
			if d.ID != 0 && d.ID <= after {
				continue
			}
			progressed = true
			if d.ID > after {
				after = d.ID
			}
			res.Decisions++
			got, err := o.rule(o.spec, d.Point, d.Inputs)
			if err != nil || got != d.Rule {
				m := Mismatch{ID: d.ID, Point: d.Point, Subject: d.Subject, Recorded: d.Rule, Replayed: got, Err: err}
				res.Mismatches = append(res.Mismatches, m)
				if o.page != nil {
					o.page(m)
				}
			}
		}
		// A reader that returns everything at once (or ids of 0) has nothing more after one page.
		if !progressed || after == 0 {
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
}
