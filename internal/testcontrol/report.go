package testcontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The environment of TestGoTestControl, which .circleci/scenario-control.sh
// sets.
const (
	// BaseEnv names a checkout of the pull request's merge base that the
	// control may change: it applies the pull request's test files there.
	BaseEnv = "BEEKEEPER_TEST_CONTROL_BASE"
	// HeadEnv names the pull request's checkout; without it, the
	// repository this package is in.
	HeadEnv = "BEEKEEPER_TEST_CONTROL_HEAD"
	// ReportEnv names a directory for report.txt, report.json and every
	// go test run's events, command and stderr.
	ReportEnv = "BEEKEEPER_TEST_CONTROL_REPORT"
)

// Options are a control's trees.
type Options struct {
	// Head is the pull request's checkout, Base its merge base's, which
	// the control changes.
	Head, Base string
	// Logs, when set, receives every go test run's events, command and
	// stderr.
	Logs string
	// Timeout is go test's -timeout for each run; DefaultTimeout without.
	Timeout time.Duration
}

// Report is the control of one pull request.
type Report struct {
	// Base and Head are the commits.
	Base        string   `json:"base"`
	Head        string   `json:"head"`
	CodeChanged bool     `json:"code_changed"`
	Applied     []string `json:"applied,omitempty"`
	Cases       []*Case  `json:"cases"`
	Problems    []string `json:"problems,omitempty"`
	OK          bool     `json:"ok"`
	Summary     string   `json:"summary"`
}

// Control runs the control of the pull request checked out at o.Head
// against its merge base checked out at o.Base: it plans the cases,
// applies the pull request's test files to the base, runs the cases there
// and those that fail there again on the pull request's code.
func Control(ctx context.Context, o Options) (*Report, error) {
	headRev, err := checkedOut(ctx, o.Head)
	if err != nil {
		return nil, err
	}
	baseRev, err := checkedOut(ctx, o.Base)
	if err != nil {
		return nil, err
	}
	changes, err := Diff(ctx, o.Head, baseRev, headRev)
	if err != nil {
		return nil, err
	}
	plan, err := PlanControl(changes, o.Head, o.Base)
	if err != nil {
		return nil, err
	}
	rep := &Report{Base: baseRev, Head: headRev, CodeChanged: plan.CodeChanged, Cases: plan.Cases, Problems: plan.Problems}
	for _, c := range plan.Overlay {
		rep.Applied = append(rep.Applied, c.String())
	}
	if run := plan.Run(); len(run) > 0 {
		if err := runCases(ctx, o, plan, run); err != nil {
			return nil, err
		}
	}
	rep.judge()
	return rep, nil
}

// checkedOut is the commit checked out at dir.
func checkedOut(ctx context.Context, dir string) (string, error) {
	rev, err := git(ctx, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(rev), err
}

// runCases applies the pull request's test files to the base, runs the
// cases there and those that fail there on the pull request's code.
func runCases(ctx context.Context, o Options, plan *Plan, run []*Case) error {
	if err := Apply(plan.Overlay, o.Head, o.Base); err != nil {
		return fmt.Errorf("applying the pull request's test files to the base: %w", err)
	}
	module, err := modulePath(o.Head)
	if err != nil {
		return err
	}
	rn := &runner{module: module, timeout: o.Timeout, logs: o.Logs}
	if rn.timeout <= 0 {
		rn.timeout = DefaultTimeout
	}
	if rn.logs != "" {
		if err := os.MkdirAll(rn.logs, 0o750); err != nil {
			return err
		}
	}
	verdicts, details, err := rn.run(ctx, o.Base, "base", run)
	if err != nil {
		return err
	}
	var failing []*Case
	for i, c := range run {
		c.Base, c.Detail = verdicts[i], details[i]
		if c.Base == Fails {
			failing = append(failing, c)
		}
	}
	if len(failing) == 0 {
		return nil
	}
	verdicts, details, err = rn.run(ctx, o.Head, "head", failing)
	if err != nil {
		return err
	}
	for i, c := range failing {
		c.Head = verdicts[i]
		if c.Head != Passes && details[i] != "" {
			c.Detail += "\non the pull request's code:\n" + details[i]
		}
	}
	return nil
}

// judge decides the control: at least one test run on the base fails
// there and passes on the pull request's code, or none is left to run.
func (rep *Report) judge() {
	var run, shows, noBuild int
	for _, c := range rep.Cases {
		switch {
		case c.Skip != "":
		case c.Shows():
			run++
			shows++
		default:
			run++
			if c.Base == NoBuild {
				noBuild++
			}
		}
	}
	switch {
	case len(rep.Problems) > 0:
		rep.Summary = fmt.Sprintf("%d control line(s) cannot be read: write control: none: <reason> in the test's doc comment", len(rep.Problems))
	case len(rep.Cases) == 0:
		rep.OK, rep.Summary = true, "the pull request adds or changes no Test function"
	case run == 0:
		rep.OK, rep.Summary = true, fmt.Sprintf("none of the %d new or changed test(s) runs on the merge base; the review checks why", len(rep.Cases))
	case shows > 0:
		rep.OK, rep.Summary = true, fmt.Sprintf("%d of %d test(s) run on the merge base fail there and pass on the pull request's code", shows, run)
	default:
		rep.Summary = fmt.Sprintf("none of the %d test(s) run on the merge base fails there and passes on the pull request's code, so none shows its change", run)
		if noBuild > 0 {
			rep.Summary += fmt.Sprintf(" (%d of them do not build there, which shows nothing)", noBuild)
		}
		rep.Summary += ". Make the fix's regression test fail without the fix, or give each test that cannot " +
			"(it covers behavior main already has, or code the base lacks) a control: none: <why> paragraph in its doc comment for the review to check"
	}
}

// Text writes the report for people.
func (rep *Report) Text(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Go-test control of %s against the merge base %s\n", short(rep.Head), short(rep.Base))
	if !rep.CodeChanged {
		b.WriteString("the pull request changes no code the Go build reads, only tests and other files\n")
	}
	if len(rep.Applied) > 0 {
		fmt.Fprintf(&b, "applied to the base: %s\n", strings.Join(rep.Applied, ", "))
	}
	for _, c := range rep.Cases {
		status := "--"
		switch {
		case c.Skip != "":
			status = "SKIP"
		case c.Shows():
			status = "OK"
		}
		fmt.Fprintf(&b, "%-4s %s %s (%s", status, c.Dir, c.Name, c.Change)
		if len(c.Tags) > 0 {
			fmt.Fprintf(&b, ", -tags %s", strings.Join(c.Tags, ","))
		}
		b.WriteString(")")
		switch {
		case c.Skip != "":
			fmt.Fprintf(&b, ": %s", c.Skip)
		case c.Base != "":
			fmt.Fprintf(&b, ": %s on the base", c.Base)
			if c.Head != "" {
				fmt.Fprintf(&b, ", %s on the pull request's code", c.Head)
			}
		}
		b.WriteString("\n")
		if c.Reason != "" {
			fmt.Fprintf(&b, "     reason: %s\n", c.Reason)
		}
		if c.Note != "" {
			fmt.Fprintf(&b, "     note: %s\n", c.Note)
		}
		if c.Detail != "" {
			fmt.Fprintf(&b, "     %s\n", strings.ReplaceAll(c.Detail, "\n", "\n     "))
		}
	}
	for _, p := range rep.Problems {
		fmt.Fprintf(&b, "PROBLEM %s\n", p)
	}
	verdict := "FAIL"
	if rep.OK {
		verdict = "OK"
	}
	fmt.Fprintf(&b, "Go-test control: %s: %s\n", verdict, rep.Summary)
	_, err := io.WriteString(w, b.String())
	return err
}

// Write writes report.txt and report.json into dir.
func (rep *Report) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	var text strings.Builder
	if err := rep.Text(&text); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte(text.String()), 0o600); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.json"), append(raw, '\n'), 0o600)
}

// short is a commit's first twelve characters.
func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
