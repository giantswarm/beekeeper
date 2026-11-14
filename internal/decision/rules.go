package decision

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

// Answers.
const (
	AnswerReady    = "ready"
	AnswerNotReady = "not-ready"
	AnswerYes      = "yes"
	AnswerNo       = "no"

	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// Default patterns, used when the Installation leaves one empty.
const (
	DefaultAcceptanceCriteria = `(?im)^#{1,4}\s*acceptance criteria`
	DefaultTargetRepo         = `(?im)^(repo|repository):\s*([\w.-]+/[\w.-]+)`
	DefaultFootprint          = `(?im)^(footprint|paths):\s*(.+)$`
)

// Keys of the inputs and fields questions carry.
const (
	keyTitle = "title"
	keyBody  = "body"
	keyRepo  = "repo"
)

var (
	readyAnswers    = []string{AnswerReady, AnswerNotReady}
	yesNo           = []string{AnswerYes, AnswerNo}
	severityAnswers = []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow}

	listItem = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+\S`)
	heading  = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)

	patterns sync.Map // pattern → *regexp.Regexp, or nil when it doesn't compile
)

func compile(pattern, def string) *regexp.Regexp {
	if pattern == "" {
		pattern = def
	}
	if re, ok := patterns.Load(pattern); ok {
		r, _ := re.(*regexp.Regexp)
		return r
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	patterns.Store(pattern, re)
	return re
}

// Mode returns the configured mode of a decision point; shadow when unset.
func Mode(spec *v1alpha1.InstallationSpec, point string) string {
	if spec != nil {
		if m, ok := spec.DecisionPoints[point]; ok && m != "" {
			return m
		}
	}
	return v1alpha1.DecisionModeShadow
}

// AcceptanceCriteria reports whether body has an acceptance-criteria heading (pattern) followed by a list
// item before the next heading.
func AcceptanceCriteria(pattern, body string) bool {
	re := compile(pattern, DefaultAcceptanceCriteria)
	if re == nil {
		return false
	}
	for _, loc := range re.FindAllStringIndex(body, -1) {
		rest := body[loc[1]:]
		// Skip the rest of the heading line.
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[i+1:]
		} else {
			continue
		}
		for _, line := range strings.Split(rest, "\n") {
			line = strings.TrimRight(line, "\r")
			if heading.MatchString(line) {
				break
			}
			if listItem.MatchString(line) {
				return true
			}
		}
	}
	return false
}

// TargetRepos returns the repos the work lands in: the issue's own repo when it lives in a code repo
// (not one of planningRepos), then every repo the body names on a target-repo line. Sorted after the
// issue's own repo, without duplicates.
func TargetRepos(ready v1alpha1.ReadySpec, issueRepo, body string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(r string) {
		k := strings.ToLower(r)
		if r == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, r)
	}
	if issueRepo != "" && !isPlanningRepo(ready.PlanningRepos, issueRepo) {
		add(issueRepo)
	}
	var named []string
	if re := compile(ready.TargetRepo, DefaultTargetRepo); re != nil {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if r := lastGroup(m); r != "" {
				named = append(named, strings.Trim(r, "`*_ "))
			}
		}
	}
	sort.Strings(named)
	for _, r := range named {
		add(r)
	}
	return out
}

func isPlanningRepo(planning []string, repo string) bool {
	for _, p := range planning {
		if strings.EqualFold(p, repo) {
			return true
		}
	}
	return false
}

// Footprint returns the paths the body names on footprint lines (comma or space separated), sorted and
// without duplicates.
func Footprint(pattern, body string) []string {
	re := compile(pattern, DefaultFootprint)
	if re == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		for _, p := range strings.FieldsFunc(lastGroup(m), func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\r'
		}) {
			p = strings.Trim(p, "`*_")
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func lastGroup(m []string) string {
	for i := len(m) - 1; i >= 1; i-- {
		if strings.TrimSpace(m[i]) != "" {
			return strings.TrimSpace(m[i])
		}
	}
	return ""
}

// ReadyFacts are what ready? is decided on.
type ReadyFacts struct {
	Title, Body string
	// Repo is the issue's repo (owner/repo).
	Repo string
	// Bug skips refinement: only the target repo is checked.
	Bug bool
}

// ReadyRule is the ready? rule: an acceptance-criteria heading with a list item (bugs skip it) and a
// known target repo.
func ReadyRule(ready v1alpha1.ReadySpec, f ReadyFacts) string {
	if len(TargetRepos(ready, f.Repo, f.Body)) == 0 {
		return AnswerNotReady
	}
	if !f.Bug && !AcceptanceCriteria(ready.AcceptanceCriteria, f.Body) {
		return AnswerNotReady
	}
	return AnswerReady
}

// ReadyInputs are ready?'s inputs: title, body and the issue's repo (never the assignees). A bug adds
// bug: true, because bugs skip refinement and the rule must be reproducible from the inputs.
func ReadyInputs(f ReadyFacts) map[string]any {
	in := map[string]any{keyTitle: f.Title, keyBody: f.Body, keyRepo: f.Repo}
	if f.Bug {
		in["bug"] = true
	}
	return in
}

// ReadyQuestion builds the ready? question for an item. JEV gets the title and the body.
func ReadyQuestion(spec *v1alpha1.InstallationSpec, team, subject string, f ReadyFacts) Question {
	in := ReadyInputs(f)
	return Question{
		Point:      PointReady,
		Subject:    subject,
		Team:       team,
		Inputs:     in,
		InputsHash: Hash(in),
		Rule:       ReadyRule(readySpec(spec), f),
		Answers:    readyAnswers,
		Mode:       Mode(spec, PointReady),
		Fields:     map[string]any{keyTitle: f.Title, keyBody: f.Body},
	}
}

// BugSeverityRule is the bug-severity? rule: medium until a person sets it.
func BugSeverityRule() string { return SeverityMedium }

// BugSeverityQuestion builds the bug-severity? question for a bug. JEV gets the title and the body.
func BugSeverityQuestion(spec *v1alpha1.InstallationSpec, team, subject, title, body string) Question {
	in := map[string]any{keyTitle: title, keyBody: body}
	return Question{
		Point:      PointBugSeverity,
		Subject:    subject,
		Team:       team,
		Inputs:     in,
		InputsHash: Hash(in),
		Rule:       BugSeverityRule(),
		Answers:    severityAnswers,
		Mode:       Mode(spec, PointBugSeverity),
		Fields:     map[string]any{keyTitle: title, keyBody: body},
	}
}

// ParallelSafeRule is the parallel-safe? rule: sequential per repo unless both items name footprints
// and they are disjoint.
func ParallelSafeRule(footprint, other []string) string {
	if len(footprint) == 0 || len(other) == 0 {
		return AnswerNo
	}
	for _, a := range footprint {
		for _, b := range other {
			if pathsOverlap(a, b) {
				return AnswerNo
			}
		}
	}
	return AnswerYes
}

// pathsOverlap reports whether one path contains the other (on path segment boundaries).
func pathsOverlap(a, b string) bool {
	a, b = cleanPath(a), cleanPath(b)
	if a == "" || b == "" || a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/**"), "/*")
	return strings.Trim(p, "/")
}

// ParallelSafeQuestion builds parallel-safe? for subject, whose repo other already takes. JEV gets the
// footprints.
func ParallelSafeQuestion(spec *v1alpha1.InstallationSpec, team, subject, other, repo string, footprint, otherFootprint []string) Question {
	fp, ofp := stringsOrEmpty(footprint), stringsOrEmpty(otherFootprint)
	in := map[string]any{keyRepo: repo, "footprint": fp, "other": other, "otherFootprint": ofp}
	return Question{
		Point:      PointParallelSafe,
		Subject:    subject,
		Other:      other,
		Team:       team,
		Inputs:     in,
		InputsHash: Hash(in),
		Rule:       ParallelSafeRule(footprint, otherFootprint),
		Answers:    yesNo,
		Mode:       Mode(spec, PointParallelSafe),
		Fields:     map[string]any{"footprint": fp, "otherFootprint": ofp},
	}
}

func stringsOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Candidate is one of a person's candidates for next?, in the team's order.
type Candidate struct {
	Ref   string `json:"ref"`
	Title string `json:"title"`
	// Priority is the board's Priority; nil when unset.
	Priority *int `json:"priority"`
	// CreatedAt is the issue's creation time (RFC 3339), empty when unknown.
	CreatedAt string `json:"createdAt"`
	// Unblocks is true for work that unblocks in-flight work.
	Unblocks bool `json:"unblocks"`
	// ActiveEpic is true for a sub-item of an Active epic.
	ActiveEpic bool `json:"activeEpic"`
	// Held is the gate that held the candidate when the person's turn came, if it did.
	Held string `json:"held,omitempty"`
}

// NextRule is the next? rule: the first candidate in the order that no gate holds; empty when none.
func NextRule(cands []Candidate) string {
	for _, c := range cands {
		if c.Held == "" {
			return c.Ref
		}
	}
	return ""
}

// NextQuestion builds next? for a person. Its inputs are the person's candidates in order (never the
// person); JEV gets the candidate summaries.
func NextQuestion(spec *v1alpha1.InstallationSpec, team, person string, cands []Candidate) Question {
	list := make([]any, 0, len(cands))
	answers := make([]string, 0, len(cands))
	for _, c := range cands {
		list = append(list, candidateMap(c))
		answers = append(answers, c.Ref)
	}
	in := map[string]any{"candidates": list}
	return Question{
		Point:      PointNext,
		Subject:    person,
		Team:       team,
		Inputs:     in,
		InputsHash: Hash(in),
		Rule:       NextRule(cands),
		Answers:    answers,
		Mode:       Mode(spec, PointNext),
		Fields:     map[string]any{"candidates": list},
	}
}

func candidateMap(c Candidate) map[string]any {
	m := map[string]any{
		"ref":        c.Ref,
		keyTitle:     c.Title,
		"createdAt":  c.CreatedAt,
		"unblocks":   c.Unblocks,
		"activeEpic": c.ActiveEpic,
	}
	if c.Priority != nil {
		m["priority"] = *c.Priority
	} else {
		m["priority"] = nil
	}
	if c.Held != "" {
		m["held"] = c.Held
	}
	return m
}

// Replay re-runs a decision point's rule on recorded inputs (as recorded, or decoded from JSON).
func Replay(spec *v1alpha1.InstallationSpec, point string, inputs map[string]any) (string, error) {
	b, err := json.Marshal(inputs)
	if err != nil {
		return "", fmt.Errorf("replay %s: %w", point, err)
	}
	switch point {
	case PointReady:
		var in struct {
			Title, Body, Repo string
			Bug               bool
		}
		if err := json.Unmarshal(b, &in); err != nil {
			return "", fmt.Errorf("replay %s: %w", point, err)
		}
		return ReadyRule(readySpec(spec), ReadyFacts(in)), nil
	case PointBugSeverity:
		return BugSeverityRule(), nil
	case PointParallelSafe:
		var in struct {
			Footprint      []string `json:"footprint"`
			OtherFootprint []string `json:"otherFootprint"`
		}
		if err := json.Unmarshal(b, &in); err != nil {
			return "", fmt.Errorf("replay %s: %w", point, err)
		}
		return ParallelSafeRule(in.Footprint, in.OtherFootprint), nil
	case PointNext:
		var in struct {
			Candidates []Candidate `json:"candidates"`
		}
		if err := json.Unmarshal(b, &in); err != nil {
			return "", fmt.Errorf("replay %s: %w", point, err)
		}
		return NextRule(in.Candidates), nil
	default:
		return "", fmt.Errorf("replay: unknown decision point %q", point)
	}
}

func readySpec(spec *v1alpha1.InstallationSpec) v1alpha1.ReadySpec {
	if spec == nil {
		return v1alpha1.ReadySpec{}
	}
	return spec.Ready
}
