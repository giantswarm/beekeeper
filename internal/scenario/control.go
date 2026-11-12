//go:build scenario

package scenario

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The control proves that a pull request's scenarios show its change: each
// scenario it adds or changes runs against the candidate built from its
// merge base and must fail there on an assertion. A scenario that passes on
// the base shows nothing the change does; one whose run on the base fails
// in the harness (the sandbox, the world, its own load) is no evidence.
// .circleci/scenario-control.sh runs it in CI; docs/scenarios.md has the
// rule.

// ControlEnv names the merge base's source tree, a checkout of the pull
// request's merge base, to TestScenarioControl.
const ControlEnv = "BEEKEEPER_SCENARIO_CONTROL_BASE"

// controlMinWords is the fewest words of an opt-out's reason.
const controlMinWords = 3

// Control is a scenario's opt-out of the control, written
// `control: {none: <reason>}`: the reason says why the scenario cannot fail
// without the change of the pull request that adds or changes it, such as
// a scenario of behavior main already has. The review checks it.
type Control struct {
	None string `yaml:"none"`
}

// UnmarshalYAML reads {none: <reason>} and nothing else: the loader's
// strictness does not reach into a type that decodes itself.
func (c *Control) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: control is {none: <reason>}: the reason the scenario cannot fail without its change", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value != "none" {
			return fmt.Errorf("line %d: field %s not found in control (only none)", k.Line, k.Value)
		}
		if err := v.Decode(&c.None); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks an opt-out's reason; a scenario without one is valid.
func (c *Control) Validate() error {
	if c == nil {
		return nil
	}
	if len(strings.Fields(c.None)) < controlMinWords {
		return fmt.Errorf("control.none %q: say in words why the scenario cannot fail without its pull request's change", c.None)
	}
	return nil
}

// How a scenario of the pull request stands to the merge base.
const (
	// ChangeAdded is a scenario the base does not have.
	ChangeAdded = "added"
	// ChangeChanged is a scenario whose given, when, then or timeout differ
	// from the base's.
	ChangeChanged = "changed"
	// ChangeMetadata is a scenario of which only name, description, issue,
	// flow, tags, pending, control, comments or layout differ: its behavior
	// is the base's, so the control does not run it.
	ChangeMetadata = "metadata only"
	// ChangeOptedOut is an added or changed scenario whose control: none
	// this pull request writes: the control does not run it, the review
	// checks the reason.
	ChangeOptedOut = "opted out"
)

// metadataKeys are a scenario's keys that do not change what it runs or
// asserts.
var metadataKeys = []string{"name", "description", "issue", "flow", "tags", "pending", "control"}

// ControlCase is one scenario the pull request adds or changes.
type ControlCase struct {
	Scenario *Scenario
	// Rel is the scenario's file under scenarios/.
	Rel    string
	Change string
	// Reason is the opt-out's reason; Note explains a case the control
	// runs although it carries one.
	Reason string
	Note   string
}

// ControlPlan is the control of one pull request: Run against the merge
// base, Skip listed for the review.
type ControlPlan struct {
	Run  []*ControlCase
	Skip []*ControlCase
}

// baseScenario is a scenario file of the merge base, decoded as plain
// YAML: the pull request's loader may not read the base's files.
type baseScenario struct {
	raw []byte
	doc map[string]any
}

// PlanControl compares the pull request's scenarios (loaded from prRoot)
// with the files under baseRoot, the merge base's scenarios/ (absent
// before the harness): a scenario is the base's of the same name, or else
// of the same file. Scenarios the pull request leaves as they are, and the
// ones it removes, are not part of the control.
//
// An opt-out holds for the change that writes it: a scenario whose control
// value is the base's runs when its given, when, then or timeout change, so
// a reason written for one change does not wave through the next.
func PlanControl(pr []*Scenario, prRoot, baseRoot string) (*ControlPlan, error) {
	byName, byPath, err := readBase(baseRoot)
	if err != nil {
		return nil, err
	}
	plan := &ControlPlan{}
	for _, s := range pr {
		rel, err := filepath.Rel(prRoot, s.File)
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(s.File) //nolint:gosec // a scenario file of the suite
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", s.File, err)
		}
		c := &ControlCase{Scenario: s, Rel: rel, Change: ChangeAdded}
		base, ok := byName[s.Name]
		if !ok {
			base, ok = byPath[rel]
		}
		switch {
		case !ok:
		case bytes.Equal(base.raw, raw):
			continue
		case base.doc != nil && sameBehavior(base.doc, doc):
			c.Change = ChangeMetadata
			plan.Skip = append(plan.Skip, c)
			continue
		default:
			c.Change = ChangeChanged
		}
		if s.Control != nil {
			c.Reason = s.Control.None
			if ok && base.doc != nil && reflect.DeepEqual(base.doc["control"], doc["control"]) {
				c.Note = "its control: none is the base's, written for an earlier change; this change runs"
			} else {
				c.Change = ChangeOptedOut + " (" + c.Change + ")"
				plan.Skip = append(plan.Skip, c)
				continue
			}
		}
		plan.Run = append(plan.Run, c)
	}
	return plan, nil
}

// readBase reads the base's scenario files by name and by file.
func readBase(root string) (byName, byPath map[string]*baseScenario, err error) {
	byName, byPath = map[string]*baseScenario{}, map[string]*baseScenario{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return fs.SkipAll
			}
			return err
		}
		if ext := filepath.Ext(path); d.IsDir() || ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // a scenario file of the merge base
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b := &baseScenario{raw: raw}
		var doc map[string]any
		if yaml.Unmarshal(raw, &doc) == nil && doc != nil {
			b.doc = doc
			if name, ok := doc["name"].(string); ok && name != "" {
				byName[name] = b
			}
		}
		byPath[rel] = b
		return nil
	})
	return byName, byPath, err
}

// sameBehavior says whether two scenario documents differ in metadata
// only.
func sameBehavior(a, b map[string]any) bool {
	strip := func(m map[string]any) map[string]any {
		out := make(map[string]any, len(m))
		for k, v := range m {
			if !slices.Contains(metadataKeys, k) {
				out[k] = v
			}
		}
		return out
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// The control's verdicts on a scenario run against the merge base.
const (
	VerdictFails   = "fails on the base"
	VerdictPasses  = "passes on the base"
	VerdictHarness = "harness failure on the base"
)

// ControlVerdict judges a scenario's run against the merge base: it must
// fail, and on an assertion. It returns the verdict and, for a failing
// control, what to do.
func ControlVerdict(c *ControlCase, r *Result) (string, error) {
	switch {
	case r.Harness != "":
		return VerdictHarness, fmt.Errorf("%s: %s: the run on the base failed in the harness, not on an assertion, so it shows nothing: %s", c.Rel, VerdictHarness, r.Harness)
	case r.Passed():
		msg := fmt.Sprintf("%s: %s: it does not show this pull request's change. Make it fail without the change, or, if it cannot (it states behavior main already has), add control: {none: <why>} for the review to check", c.Rel, VerdictPasses)
		if c.Note != "" {
			msg += "; " + c.Note
		}
		return VerdictPasses, errors.New(msg)
	}
	return VerdictFails, nil
}

// ControlEntry is one scenario in the control's report.
type ControlEntry struct {
	Name    string `json:"name"`
	Flow    string `json:"flow"`
	File    string `json:"file"`
	Change  string `json:"change"`
	Reason  string `json:"reason,omitempty"`
	Note    string `json:"note,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	// Detail is the first failures on the base, or the problem.
	Detail string `json:"detail,omitempty"`
	OK     bool   `json:"ok"`
}

// ControlReport is the control of one pull request.
type ControlReport struct {
	Base    string          `json:"base"`
	Entries []*ControlEntry `json:"entries"`
}

// Entry adds the case to the report.
func (rep *ControlReport) Entry(c *ControlCase) *ControlEntry {
	e := &ControlEntry{Name: c.Scenario.Name, Flow: c.Scenario.Flow, File: c.Rel, Change: c.Change, Reason: c.Reason, Note: c.Note}
	rep.Entries = append(rep.Entries, e)
	return e
}

// Text writes the report for people.
func (rep *ControlReport) Text(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "scenario control against %s\n", rep.Base)
	if len(rep.Entries) == 0 {
		b.WriteString("no scenario added or changed\n")
	}
	failed := 0
	for _, e := range rep.Entries {
		status := "OK"
		if !e.OK {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(&b, "%-4s %s (%s)", status, e.File, e.Change)
		if e.Verdict != "" {
			fmt.Fprintf(&b, ": %s", e.Verdict)
		}
		b.WriteString("\n")
		if e.Reason != "" {
			fmt.Fprintf(&b, "     reason: %s\n", e.Reason)
		}
		if e.Note != "" {
			fmt.Fprintf(&b, "     note: %s\n", e.Note)
		}
		if e.Detail != "" {
			fmt.Fprintf(&b, "     %s\n", strings.ReplaceAll(e.Detail, "\n", "\n     "))
		}
	}
	fmt.Fprintf(&b, "control: %d scenario(s), %d failing\n", len(rep.Entries), failed)
	_, err := io.WriteString(w, b.String())
	return err
}

// Write writes report.txt and report.json into dir.
func (rep *ControlReport) Write(dir string) error {
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

// firstFailures is the first n failures of a run, for the report.
func firstFailures(r *Result, n int) string {
	return strings.Join(r.Failures[:min(n, len(r.Failures))], "\n")
}
