package decision

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

// Fixtures: repositories, items and footprint paths.
const (
	repoKlaus     = "giantswarm/klaus"
	repoRoadmap   = "giantswarm/roadmap"
	refRoadmap101 = "giantswarm/roadmap#101"
	refRoadmap102 = "giantswarm/roadmap#102"
	pathClient    = "pkg/client"
	pathMetrics   = "pkg/metrics"
)

func spec() *v1alpha1.InstallationSpec {
	return &v1alpha1.InstallationSpec{
		DecisionPoints: map[string]string{
			PointReady: v1alpha1.DecisionModeShadow, PointNext: v1alpha1.DecisionModeShadow,
			PointParallelSafe: v1alpha1.DecisionModeShadow, PointBugSeverity: v1alpha1.DecisionModeShadow,
		},
		Ready: v1alpha1.ReadySpec{
			AcceptanceCriteria: `(?im)^#{1,4}\s*acceptance criteria`,
			TargetRepo:         `(?im)^(repo|repository):\s*([\w.-]+/[\w.-]+)`,
			PlanningRepos:      []string{repoRoadmap, "giantswarm/giantswarm"},
			Todo:               "refine with `/gs-plans:gs-plan`",
		},
		Footprint: `(?im)^(footprint|paths):\s*(.+)$`,
	}
}

func TestReadyRule(t *testing.T) {
	t.Parallel()
	ac := "### Acceptance criteria\n- [ ] retries 5xx up to three times\n"
	cases := []struct {
		name    string
		facts   ReadyFacts
		want    string
		ac      bool
		targets []string
	}{
		{"eligibility-up-next-ready-item-is-eligible", ReadyFacts{Body: ac + "Repo: giantswarm/klaus\n", Repo: repoRoadmap}, AnswerReady, true, []string{repoKlaus}},
		{"eligibility-issue-in-code-repo-has-target", ReadyFacts{Body: ac, Repo: repoKlaus}, AnswerReady, true, []string{repoKlaus}},
		{"eligibility-no-target-repo-not-ready", ReadyFacts{Body: ac, Repo: repoRoadmap}, AnswerNotReady, true, nil},
		{"eligibility-not-ready-records-one-todo", ReadyFacts{Body: "The client should retry.\nRepo: giantswarm/klaus\n", Repo: repoRoadmap}, AnswerNotReady, false, []string{repoKlaus}},
		{"eligibility-becomes-ready-when-body-edited", ReadyFacts{Body: "The client should retry.\n### Acceptance criteria\n- [ ] retries\nRepo: giantswarm/klaus\n", Repo: repoRoadmap}, AnswerReady, true, []string{repoKlaus}},
		{"heading without a list item", ReadyFacts{Body: "## Acceptance Criteria\nTo be written.\n## Notes\n- not a criterion\nRepo: giantswarm/klaus\n", Repo: repoRoadmap}, AnswerNotReady, false, []string{repoKlaus}},
		{"numbered list and CRLF", ReadyFacts{Body: "#### acceptance criteria\r\n1. it works\r\nRepository: giantswarm/muster\r\n", Repo: repoRoadmap}, AnswerReady, true, []string{"giantswarm/muster"}},
		{"bugs skip refinement", ReadyFacts{Body: "It crashes.\nRepo: giantswarm/klaus\n", Repo: repoRoadmap, Bug: true}, AnswerReady, false, []string{repoKlaus}},
		{"a bug still needs a target repo", ReadyFacts{Body: "It crashes.\n", Repo: repoRoadmap, Bug: true}, AnswerNotReady, false, nil},
	}
	s := spec()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ReadyRule(s.Ready, c.facts); got != c.want {
				t.Errorf("ReadyRule = %q, want %q", got, c.want)
			}
			if got := AcceptanceCriteria(s.Ready.AcceptanceCriteria, c.facts.Body); got != c.ac {
				t.Errorf("AcceptanceCriteria = %v, want %v", got, c.ac)
			}
			if got := TargetRepos(s.Ready, c.facts.Repo, c.facts.Body); !reflect.DeepEqual(got, c.targets) {
				t.Errorf("TargetRepos = %v, want %v", got, c.targets)
			}
		})
	}
}

func TestReadyQuestion(t *testing.T) {
	t.Parallel()
	f := ReadyFacts{Title: "Retry transient errors", Body: "### Acceptance criteria\n- [ ] x\nRepo: giantswarm/klaus\n", Repo: repoRoadmap}
	cases := []struct {
		name     string
		mode     string
		facts    ReadyFacts
		wantMode string
	}{
		{"decision-ready-shadow-records-both", v1alpha1.DecisionModeShadow, f, v1alpha1.DecisionModeShadow},
		{"decision-mode-rule-skips-jev", "rule", f, "rule"},
		{"unset mode is shadow", "", f, v1alpha1.DecisionModeShadow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := spec()
			if c.mode == "" {
				delete(s.DecisionPoints, PointReady)
			} else {
				s.DecisionPoints[PointReady] = c.mode
			}
			q := ReadyQuestion(s, "bumblebee", refRoadmap101, c.facts)
			if q.Mode != c.wantMode || q.Rule != AnswerReady || q.Point != PointReady || q.Subject != refRoadmap101 {
				t.Errorf("question = %+v", q)
			}
			// decision-jev-gets-only-needed-fields: JEV gets title and body, nothing else.
			if !reflect.DeepEqual(q.Fields, map[string]any{keyTitle: f.Title, keyBody: f.Body}) {
				t.Errorf("Fields = %v", q.Fields)
			}
			// The inputs are title, body and repo: never the assignees.
			if !reflect.DeepEqual(q.Inputs, map[string]any{keyTitle: f.Title, keyBody: f.Body, keyRepo: f.Repo}) {
				t.Errorf("Inputs = %v", q.Inputs)
			}
			if q.InputsHash != Hash(q.Inputs) {
				t.Error("InputsHash isn't the hash of the inputs")
			}
		})
	}
	// decision-jev-asked-once-per-inputs: same inputs, same hash; an edited body, a new one.
	a := ReadyQuestion(spec(), "bumblebee", refRoadmap101, f)
	b := ReadyQuestion(spec(), "bumblebee", refRoadmap101, f)
	edited := f
	edited.Body += "- [ ] gives up after 30s in total\n"
	c := ReadyQuestion(spec(), "bumblebee", refRoadmap101, edited)
	if a.InputsHash != b.InputsHash || a.InputsHash == c.InputsHash {
		t.Errorf("hashes: %s %s %s", a.InputsHash, b.InputsHash, c.InputsHash)
	}
}

func TestParallelSafeRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		a, b        []string
		want        string
		wantFPBody  string
		wantFPPaths []string
	}{
		{"gate-repo-concurrency-disjoint-footprints-pass", []string{pathClient}, []string{pathMetrics}, AnswerYes, "Footprint: pkg/client\n", []string{pathClient}},
		{"gate-repo-concurrency-sequential-by-default", nil, nil, AnswerNo, "Repo: giantswarm/klaus\n", nil},
		{"only one names a footprint", []string{pathClient}, nil, AnswerNo, "", nil},
		{"nested paths overlap", []string{"pkg"}, []string{pathMetrics}, AnswerNo, "Paths: pkg, cmd/serve\n", []string{"cmd/serve", "pkg"}},
		{"same path", []string{"./pkg/client/"}, []string{pathClient}, AnswerNo, "footprint: `pkg/client` `docs`\n", []string{"docs", pathClient}},
		{"prefix that isn't a parent", []string{pathClient}, []string{"pkg/clientv2"}, AnswerYes, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ParallelSafeRule(c.a, c.b); got != c.want {
				t.Errorf("ParallelSafeRule(%v, %v) = %q, want %q", c.a, c.b, got, c.want)
			}
			if got := ParallelSafeRule(c.b, c.a); got != c.want {
				t.Errorf("not symmetric")
			}
			if got := Footprint(spec().Footprint, c.wantFPBody); !reflect.DeepEqual(got, c.wantFPPaths) {
				t.Errorf("Footprint(%q) = %v, want %v", c.wantFPBody, got, c.wantFPPaths)
			}
		})
	}
	q := ParallelSafeQuestion(spec(), "bumblebee", refRoadmap102, refRoadmap101, repoKlaus, []string{pathMetrics}, []string{pathClient})
	if q.Other != refRoadmap101 || q.Rule != AnswerYes || q.Mode != v1alpha1.DecisionModeShadow {
		t.Errorf("question = %+v", q)
	}
	if !reflect.DeepEqual(q.Fields, map[string]any{"footprint": []string{pathMetrics}, "otherFootprint": []string{pathClient}}) {
		t.Errorf("Fields = %v", q.Fields)
	}
}

func TestNextRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cands []Candidate
		want  string
	}{
		{"decision-next-shadow-recorded", []Candidate{{Ref: refRoadmap101}, {Ref: refRoadmap102}}, refRoadmap101},
		{"held candidates are skipped", []Candidate{{Ref: refRoadmap102, Held: "RepoConcurrency"}, {Ref: "giantswarm/roadmap#103"}}, "giantswarm/roadmap#103"},
		{"nothing startable", []Candidate{{Ref: refRoadmap102, Held: "TeamCap"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			q := NextQuestion(spec(), "bumblebee", "alice", c.cands)
			if q.Rule != c.want || NextRule(c.cands) != c.want {
				t.Errorf("rule = %q, want %q", q.Rule, c.want)
			}
			if q.Subject != "alice" || q.Point != PointNext {
				t.Errorf("question = %+v", q)
			}
			b, _ := json.Marshal(q.Inputs)
			if string(b) == "" || strings.Contains(string(b), "alice") {
				t.Errorf("inputs name the person: %s", b)
			}
		})
	}
}

// TestReplay: decision-replay-reproduces-every-decision. Every rule re-runs on its inputs as stored
// (JSON round trip, as Postgres jsonb gives them back) and reproduces the recorded answer.
func TestReplay(t *testing.T) {
	t.Parallel()
	s := spec()
	prio := 2
	qs := []Question{
		ReadyQuestion(s, "bumblebee", refRoadmap101, ReadyFacts{Title: "t", Body: "### Acceptance criteria\n- [ ] x\nRepo: giantswarm/klaus\n", Repo: repoRoadmap}),
		ReadyQuestion(s, "bumblebee", refRoadmap102, ReadyFacts{Title: "t", Body: "nothing\n", Repo: repoRoadmap}),
		ReadyQuestion(s, "bumblebee", "giantswarm/roadmap#205", ReadyFacts{Title: "bug", Body: "Repo: giantswarm/klaus\n", Repo: repoRoadmap, Bug: true}),
		BugSeverityQuestion(s, "bumblebee", "giantswarm/roadmap#130", "t", "b"),
		ParallelSafeQuestion(s, "bumblebee", refRoadmap102, refRoadmap101, repoKlaus, nil, []string{pathClient}),
		ParallelSafeQuestion(s, "bumblebee", refRoadmap102, refRoadmap101, repoKlaus, []string{"pkg/a"}, []string{"pkg/b"}),
		NextQuestion(s, "bumblebee", "alice", []Candidate{
			{Ref: "giantswarm/roadmap#120", Held: "TeamCap", Unblocks: true},
			{Ref: refRoadmap101, Title: "t", Priority: &prio, CreatedAt: "2026-11-10T10:00:00Z"},
			{Ref: refRoadmap102},
		}),
	}
	for _, q := range qs {
		t.Run(q.Point+" "+q.Subject, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(q.Inputs)
			if err != nil {
				t.Fatal(err)
			}
			var stored map[string]any
			if err := json.Unmarshal(b, &stored); err != nil {
				t.Fatal(err)
			}
			if Hash(stored) != q.InputsHash {
				t.Errorf("the stored inputs hash differently")
			}
			got, err := Replay(s, q.Point, stored)
			if err != nil {
				t.Fatal(err)
			}
			if got != q.Rule {
				t.Errorf("Replay = %q, recorded %q", got, q.Rule)
			}
		})
	}
	if _, err := Replay(s, "culprit?", nil); err == nil {
		t.Error("unknown point replayed")
	}
}

func TestOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		rule, jev  string
		confidence float64
		err        error
		want       string
	}{
		{"decision-ready-shadow-records-both", AnswerReady, "not-ready", 0.8, nil, OutcomeDisagree},
		{"decision-jev-low-confidence-recorded", AnswerReady, "not-ready", 0.3, nil, OutcomeLowConfidence},
		{"decision-jev-error-rule-stands", AnswerReady, "", 0, errors.New("timeout"), OutcomeJEVError},
		{"agree", "medium", "medium", 0.9, nil, OutcomeAgree},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Outcome(c.rule, c.jev, c.confidence, c.err, 0.5); got != c.want {
				t.Errorf("Outcome = %q, want %q", got, c.want)
			}
		})
	}
}
