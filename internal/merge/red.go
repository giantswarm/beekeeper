package merge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

// securityScan matches the names of the security scans' checks, which a red
// outcome always refuses: a scan red on the base head too is a finding
// nobody has answered, never a known breakage.
var securityScan = regexp.MustCompile(`(?i)nancy|gitleaks|govulncheck|trivy|grype|snyk|codeql|vuln|security|secret.?scan`)

// SecurityScan says whether the named check is a security scan.
func SecurityScan(name string) bool { return securityScan.MatchString(name) }

// Accepted is a red check the gate lets through, and why.
type Accepted struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

// RedJudgement is the gate's judgement of a red devctl pr merge document
// against its base: the red checks it lets through and the reasons it
// refuses the rest.
type RedJudgement struct {
	Accepted []Accepted `json:"accepted,omitempty"`
	Refused  []string   `json:"refused,omitempty"`
	// Infra are the refused checks that died on a transient infrastructure
	// failure, not on the pull request's change.
	Infra []Infra `json:"infra,omitempty"`
}

// LetThrough says whether every red check of the document is let through.
func (j RedJudgement) LetThrough() bool { return len(j.Accepted) > 0 && len(j.Refused) == 0 }

// Names are the checks the judgement lets through.
func (j RedJudgement) Names() []string {
	names := make([]string, 0, len(j.Accepted))
	for _, a := range j.Accepted {
		names = append(names, a.Name)
	}
	return names
}

// String is the judgement in one line: what is let through and what refuses.
func (j RedJudgement) String() string {
	var parts []string
	for _, a := range j.Accepted {
		parts = append(parts, fmt.Sprintf("%s let through: %s", a.Name, a.Why))
	}
	return strings.Join(append(parts, j.Refused...), "; ")
}

// redCheck is one failed check of devctl's document.
type redCheck struct {
	name     string
	url      string
	required bool
	workflow bool
}

// redChecks are the failed checks and CircleCI workflows of devctl's
// document, each once, ok false when it is no document.
func redChecks(doc []byte) (reds []redCheck, ok bool) {
	var d struct {
		Checks []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			Required   bool   `json:"required"`
			URL        string `json:"url"`
		} `json:"checks"`
		CircleCI *struct {
			Workflows []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"workflows"`
		} `json:"circleci"`
	}
	if json.Unmarshal(doc, &d) != nil {
		return nil, false
	}
	failed := func(c string) bool { return slices.Contains(failedConclusions, strings.ToLower(c)) }
	for _, c := range d.Checks {
		i := slices.IndexFunc(reds, func(r redCheck) bool { return r.name == c.Name })
		switch {
		case !failed(c.Conclusion):
		case i < 0:
			reds = append(reds, redCheck{name: c.Name, url: c.URL, required: c.Required})
		default:
			reds[i].required = reds[i].required || c.Required
		}
	}
	if d.CircleCI != nil {
		for _, w := range d.CircleCI.Workflows {
			if failed(w.Status) {
				reds = append(reds, redCheck{name: "circleci workflow " + w.Name, workflow: true})
			}
		}
	}
	return reds, true
}

// JudgeRed judges devctl pr merge's red document against its base: a red
// check that is neither required (by the document or base) nor a security
// scan, and is red on the base head too, is let through, named with why; any
// other red check refuses the merge, as does a red CircleCI workflow (a
// pipeline, judged by none of its jobs' names), a check green on the base
// head (the pull request broke it), and one the base head has no finished
// result for, so nothing slips through on a missing comparison. A refused
// check infra names (InfraReds) is refused as a transient infrastructure
// failure with its log line and rerun, not as the pull request's breakage.
func JudgeRed(doc []byte, base github.BaseChecks, infra []Infra) RedJudgement {
	var j RedJudgement
	reds, ok := redChecks(doc)
	if !ok {
		return RedJudgement{Refused: []string{"devctl's document does not read"}}
	}
	at := fmt.Sprintf("the head %.7s of %s", base.Head, base.Base)
	for _, r := range reds {
		refuse := func(why string) {
			if i := infraOf(infra, r.name); i != nil && !r.workflow {
				j.Infra = append(j.Infra, *i)
				why = i.Why()
			}
			j.Refused = append(j.Refused, fmt.Sprintf("%s refuses: %s", r.name, why))
		}
		switch o := base.Outcome(r.name); {
		case r.workflow:
			refuse("a red CircleCI workflow")
		case SecurityScan(r.name):
			refuse("a security scan")
		case r.required || base.Requires(r.name):
			refuse("required by " + base.Base)
		case o == github.CheckFail:
			j.Accepted = append(j.Accepted, Accepted{Name: r.name, Why: "not required, and red on " + at + " too: not this pull request's breakage"})
		case o == github.CheckPass:
			refuse("green on " + at + ": this pull request broke it")
		case o == github.CheckPending:
			refuse("still running on " + at)
		default:
			refuse("no result on " + at)
		}
	}
	return j
}

// Red says whether devctl's document is a red verdict (exit 1): a failed
// check, status, run or workflow.
func Red(doc []byte) bool {
	var d struct {
		ExitCode int    `json:"exitCode"`
		Verdict  string `json:"verdict"`
	}
	return json.Unmarshal(doc, &d) == nil && d.ExitCode == 1 && d.Verdict == "red"
}

// BaseRef is the base branch devctl's document names, "" when none.
func BaseRef(doc []byte) string {
	var d struct {
		BaseRef string `json:"baseRef"`
	}
	_ = json.Unmarshal(doc, &d)
	return d.BaseRef
}

// AcceptRedFlag lets a named red check through devctl pr merge.
const AcceptRedFlag = "--accept-red"

// AcceptRed is a devctl pr merge argument vector that lets the named red
// checks through: argv with --accept-red <check> for each after its
// subcommand.
func AcceptRed(argv []string, checks []string) []string {
	i := slices.Index(argv, "merge")
	if i < 0 || len(checks) == 0 {
		return argv
	}
	flags := make([]string, 0, 2*len(checks))
	for _, c := range checks {
		flags = append(flags, AcceptRedFlag, c)
	}
	return slices.Insert(slices.Clone(argv), i+1, flags...)
}
