//go:build scenario

package scenario

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Result is one scenario's run as its driver saw it.
type Result struct {
	Name    string `json:"name"`
	Flow    string `json:"flow"`
	File    string `json:"file"`
	Pending string `json:"pending,omitempty"`
	// Harness is a failure of the harness itself (the sandbox, the world,
	// the desk's setup): it fails the run even for a pending scenario.
	Harness  string        `json:"harness,omitempty"`
	Failures []string      `json:"failures,omitempty"`
	Steps    []StepResult  `json:"steps"`
	Calls    []CallRecord  `json:"calls,omitempty"`
	Duration time.Duration `json:"duration"`
	// Status and Problem are the outcome, set by Outcome.
	Status  string `json:"status"`
	Problem string `json:"problem,omitempty"`
}

// StepResult is one step's run; the then expectations are the last one,
// of kind "then".
type StepResult struct {
	Index    int           `json:"index"`
	Name     string        `json:"name,omitempty"`
	Kind     string        `json:"kind"`
	Args     []string      `json:"args,omitempty"`
	Exit     *int          `json:"exit,omitempty"`
	Duration time.Duration `json:"duration"`
	Failures []string      `json:"failures,omitempty"`
}

// Passed says whether every assertion held.
func (r *Result) Passed() bool { return r.Harness == "" && len(r.Failures) == 0 }

// Fail records an assertion failure.
func (r *Result) Fail(format string, args ...any) {
	r.Failures = append(r.Failures, fmt.Sprintf(format, args...))
}

// The outcomes of a scenario.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusPending = "pending"
)

// Outcome sets and returns the scenario's status and whether it fails the
// run: a pending scenario is expected to fail, and one that passes fails
// the run until its pending marker is removed. A harness failure fails the
// run either way.
func Outcome(r *Result) (string, error) {
	switch {
	case r.Harness != "":
		r.Status = StatusFailed
		r.Problem = "harness: " + r.Harness
	case r.Pending == "" && r.Passed():
		r.Status = StatusPassed
	case r.Pending == "":
		r.Status = StatusFailed
		r.Problem = strings.Join(r.Failures, "\n")
	case r.Passed():
		r.Status = StatusFailed
		r.Problem = fmt.Sprintf("passes although pending on %s: remove the pending marker with the fix", r.Pending)
	default:
		r.Status = StatusPending
		return r.Status, nil
	}
	if r.Status == StatusFailed {
		return r.Status, errors.New(r.Problem)
	}
	return r.Status, nil
}

// Report is the suite's run.
type Report struct {
	Started  time.Time     `json:"started"`
	Wall     time.Duration `json:"wall"`
	Budget   time.Duration `json:"budget"`
	Warn     time.Duration `json:"warn"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Pending  int           `json:"pending"`
	Results  []*Result     `json:"results"`
	Problems []string      `json:"problems,omitempty"`
}

// Count tallies the outcomes.
func (rep *Report) Count() {
	rep.Passed, rep.Failed, rep.Pending = 0, 0, 0
	for _, r := range rep.Results {
		switch r.Status {
		case StatusPassed:
			rep.Passed++
		case StatusPending:
			rep.Pending++
		default:
			rep.Failed++
		}
	}
}

// Text writes the report for people.
func (rep *Report) Text(w io.Writer) error {
	var b strings.Builder
	for _, r := range rep.Results {
		fmt.Fprintf(&b, "%-8s %s/%s (%s)\n", strings.ToUpper(r.Status), r.Flow, r.Name, r.Duration.Round(time.Millisecond))
		for _, s := range r.Steps {
			label := s.Kind
			if s.Name != "" {
				label += " " + s.Name
			}
			exit := ""
			if s.Exit != nil {
				exit = fmt.Sprintf(" exit %d", *s.Exit)
			}
			fmt.Fprintf(&b, "         %2d %-30s %10s%s\n", s.Index, label, s.Duration.Round(time.Millisecond), exit)
			for _, f := range s.Failures {
				fmt.Fprintf(&b, "            ! %s\n", indent(f))
			}
		}
		if r.Problem != "" {
			fmt.Fprintf(&b, "         problem: %s\n", indent(r.Problem))
		}
	}
	fmt.Fprintf(&b, "scenarios: %d passed, %d failed, %d pending; wall time %s (budget %s, warning from %s)\n",
		rep.Passed, rep.Failed, rep.Pending, rep.Wall.Round(time.Second), rep.Budget, rep.Warn)
	for _, p := range rep.Problems {
		fmt.Fprintf(&b, "%s\n", p)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n              ") }

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Name    string       `xml:"name,attr"`
	Tests   int          `xml:"tests,attr"`
	Fails   int          `xml:"failures,attr"`
	Skipped int          `xml:"skipped,attr"`
	Time    string       `xml:"time,attr"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name    string      `xml:"name,attr"`
	Tests   int         `xml:"tests,attr"`
	Fails   int         `xml:"failures,attr"`
	Skipped int         `xml:"skipped,attr"`
	Time    string      `xml:"time,attr"`
	Cases   []junitCase `xml:"testcase"`
}

type junitCase struct {
	Class   string        `xml:"classname,attr"`
	Name    string        `xml:"name,attr"`
	Time    string        `xml:"time,attr"`
	Failure *junitMessage `xml:"failure,omitempty"`
	Skipped *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.3f", d.Seconds()) }

// JUnit writes the report as JUnit XML: one suite per scenario, one case
// per step with its duration, and one for the scenario's outcome.
func (rep *Report) JUnit(w io.Writer) error {
	all := junitSuites{Name: "beekeeper-scenarios", Time: seconds(rep.Wall)}
	for _, r := range rep.Results {
		class := r.Flow + "." + r.Name
		s := junitSuite{Name: r.Flow + "/" + r.Name, Time: seconds(r.Duration)}
		for _, st := range r.Steps {
			name := fmt.Sprintf("%02d %s", st.Index, st.Kind)
			if st.Name != "" {
				name += " " + st.Name
			}
			s.Cases = append(s.Cases, junitCase{Class: class, Name: name, Time: seconds(st.Duration)})
		}
		outcome := junitCase{Class: class, Name: "outcome", Time: seconds(r.Duration)}
		switch r.Status {
		case StatusFailed:
			outcome.Failure = &junitMessage{Message: firstLine(r.Problem), Text: r.Problem}
			s.Fails++
		case StatusPending:
			outcome.Skipped = &junitMessage{Message: "pending on " + r.Pending}
			s.Skipped++
		}
		s.Cases = append(s.Cases, outcome)
		s.Tests = len(s.Cases)
		all.Tests += s.Tests
		all.Fails += s.Fails
		all.Skipped += s.Skipped
		all.Suites = append(all.Suites, s)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(all); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Write writes report.txt, report.json and junit/scenarios.xml into dir.
func (rep *Report) Write(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "junit"), 0o750); err != nil {
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
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	var x strings.Builder
	if err := rep.JUnit(&x); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "junit", "scenarios.xml"), []byte(x.String()), 0o600)
}

// sortResults orders results by flow and name.
func sortResults(rs []*Result) {
	slices.SortFunc(rs, func(a, b *Result) int { return strings.Compare(a.Flow+"/"+a.Name, b.Flow+"/"+b.Name) })
}
