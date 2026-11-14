package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// pollInterval is how often wait_for_state re-runs a step.
const pollInterval = 50 * time.Millisecond

// StepResult is one step's outcome.
type StepResult struct {
	Step     *Step
	Result   any
	Err      error
	Failures []string
	Polls    int
	Duration time.Duration
}

// Failed reports whether the step failed.
func (r *StepResult) Failed() bool { return len(r.Failures) > 0 }

// Result is a scenario's outcome.
type Result struct {
	Scenario *Scenario
	Steps    []StepResult
	// Post are the post-check failures (contract §3.6).
	Post     []string
	Duration time.Duration
}

// Failed reports whether a step or a post-check failed.
func (r *Result) Failed() bool {
	for i := range r.Steps {
		if r.Steps[i].Failed() {
			return true
		}
	}
	return len(r.Post) > 0
}

// Report describes the failures.
func (r *Result) Report() string {
	var b strings.Builder
	for i := range r.Steps {
		s := &r.Steps[i]
		if !s.Failed() {
			continue
		}
		fmt.Fprintf(&b, "step %d %q (%s", i, s.Step.ID, s.Step.Tool)
		if s.Polls > 1 {
			fmt.Fprintf(&b, ", %d polls", s.Polls)
		}
		b.WriteString(")")
		if s.Step.Description != "" {
			fmt.Fprintf(&b, ": %s", s.Step.Description)
		}
		b.WriteString("\n")
		if len(s.Step.Args) > 0 {
			fmt.Fprintf(&b, "  args: %s\n", render(s.Step.Args))
		}
		for _, f := range s.Failures {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
		if s.Err == nil && s.Result != nil {
			fmt.Fprintf(&b, "  result: %s\n", abbreviate(render(s.Result)))
		}
	}
	for _, p := range r.Post {
		fmt.Fprintf(&b, "post-check: %s\n", p)
	}
	return b.String()
}

// Summary is the first failure in one line ("" when the scenario passed).
func (r *Result) Summary() string {
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.Failed() {
			return oneLine(fmt.Sprintf("step %s (%s): %s", s.Step.ID, s.Step.Tool, s.Failures[0]))
		}
	}
	if len(r.Post) > 0 {
		return oneLine("post-check " + r.Post[0])
	}
	return ""
}

// oneLine shortens s to one line of at most 240 characters.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// Run runs the scenario's steps against w, stopping at the first failed step, then the post-checks.
func Run(ctx context.Context, w *World) *Result {
	t0 := time.Now()
	res := &Result{Scenario: w.Scenario}
	if w.tracked == nil {
		w.tracked = trackedKeys(w.Scenario)
	}
	for i := range w.Scenario.Steps {
		sr := w.runStep(ctx, &w.Scenario.Steps[i])
		res.Steps = append(res.Steps, sr)
		if sr.Failed() || ctx.Err() != nil {
			break
		}
	}
	res.Post = w.postChecks(ctx)
	res.Duration = time.Since(t0)
	return res
}

func (w *World) runStep(ctx context.Context, st *Step) (sr StepResult) {
	t0 := time.Now()
	sr.Step = st
	defer func() { sr.Duration = time.Since(t0) }()

	wait := st.Expected.wait()
	timeout := st.timeout
	if timeout == 0 {
		timeout = stepTimeout
	}
	if wait+w.bound() > timeout {
		timeout = wait + w.bound()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := w.MarkStep(ctx, st.ID); err != nil {
		sr.Failures = []string{err.Error()}
		return sr
	}
	tool, ok := Lookup(st.Tool)
	if !ok {
		sr.Failures = []string{fmt.Sprintf("unknown tool %q", st.Tool)}
		return sr
	}
	deadline := time.Now().Add(wait)
	for {
		sr.Polls++
		sr.Result, sr.Err = tool.Run(ctx, w, copyArgs(st.Args))
		sr.Failures = st.Expected.Evaluate(sr.Result, sr.Err)
		if len(sr.Failures) == 0 || wait == 0 || ctx.Err() != nil || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
		// Between polls the system may be mid-handover: settling (within the wait) only saves polls.
		pctx, pcancel := context.WithDeadline(ctx, deadline)
		_ = w.Settle(pctx)
		pcancel()
	}
	if ctx.Err() != nil && len(sr.Failures) > 0 {
		sr.Failures = append(sr.Failures, fmt.Sprintf("step timed out after %s", timeout))
	}
	if err := w.Settle(ctx); err != nil && on(w.Scenario.PostChecks.Settled) {
		sr.Failures = append(sr.Failures, err.Error())
	}
	return sr
}

func copyArgs(a map[string]any) map[string]any {
	if a == nil {
		return map[string]any{}
	}
	return clone(a).(map[string]any)
}

// postChecks are the checks made after every scenario (contract §3.6).
func (w *World) postChecks(ctx context.Context) []string {
	pc := w.Scenario.PostChecks
	var out []string
	if err := w.Settle(ctx); err != nil && on(pc.Settled) {
		out = append(out, "settled: after the last step: "+err.Error())
	}
	// no_github_writes can't be switched off.
	out = append(out, w.noGitHubWrites(ctx)...)
	if on(pc.NoReconcileErrors) {
		v, _, err := w.metric("engine_reconcile_errors_total", nil)
		if err != nil {
			out = append(out, fmt.Sprintf("no_reconcile_errors: %v", err))
		} else if v != 0 {
			out = append(out, fmt.Sprintf("no_reconcile_errors: %s reconcile errors", fmtNum(v)))
		}
	}
	if on(pc.InvariantsHold) {
		series, err := w.series("engine_invariant_violations", nil)
		if err != nil {
			out = append(out, fmt.Sprintf("invariants_hold: %v", err))
		}
		for _, s := range series {
			inv := s.labels["invariant"]
			if s.value != 0 && !in(inv, w.Scenario.ExpectViolations) {
				out = append(out, fmt.Sprintf("invariants_hold: engine_invariant_violations{invariant=%q} is %s", inv, fmtNum(s.value)))
			}
		}
	}
	return out
}

func (w *World) noGitHubWrites(ctx context.Context) []string {
	if w.GitHub == nil {
		if w.stub {
			return nil
		}
		return []string{"no_github_writes: no fake GitHub to check"}
	}
	calls, err := w.GitHub.Do(ctx, "github_calls", map[string]any{})
	if err != nil {
		return []string{fmt.Sprintf("no_github_writes: %v", err)}
	}
	doc, err := Normalize(calls)
	if err != nil {
		return []string{fmt.Sprintf("no_github_writes: %v", err)}
	}
	v, found, _ := resolve(doc, "mutating")
	if !found {
		return []string{"no_github_writes: github_calls has no mutating count"}
	}
	if n, _ := toFloat(v); n != 0 {
		return []string{fmt.Sprintf("no_github_writes: %s mutating calls", fmtNum(n))}
	}
	return nil
}

// errNotFound marks a read of an object that doesn't exist.
var errNotFound = errors.New("not found")
