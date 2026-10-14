package merge

import (
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

// e2e and build are the cases' checks, mergeVerb devctl's subcommand.
const (
	e2e       = "e2e-test"
	build     = "go-build"
	mergeVerb = "merge"
)

// redDocOf is devctl pr merge's red document with checks, each
// "name|conclusion" or "name|conclusion|required".
func redDocOf(checks ...string) []byte {
	var parts []string
	for _, c := range checks {
		f := strings.Split(c, "|")
		parts = append(parts, `{"name":"`+f[0]+`","conclusion":"`+f[1]+`","required":`+
			map[bool]string{true: "true", false: "false"}[len(f) > 2]+`}`)
	}
	return []byte(`{"exitCode":1,"verdict":"red","checks":[` + strings.Join(parts, ",") + `]}`)
}

func TestJudgeRed(t *testing.T) {
	base := func(required []string, outcomes map[string]string) github.BaseChecks {
		return github.BaseChecks{Base: "main", Head: "0123456789abcdef", Required: required, Outcomes: outcomes}
	}
	cases := []struct {
		name     string
		doc      []byte
		base     github.BaseChecks
		infra    []Infra
		through  bool
		accepted []string
		refused  string
	}{
		{
			name:     "non-required red on both heads merges",
			doc:      redDocOf(e2e+"|failure", build+"|success|required"),
			base:     base([]string{build}, map[string]string{e2e: github.CheckFail, build: github.CheckPass}),
			through:  true,
			accepted: []string{e2e},
		},
		{
			name:    "non-required red on the pull request only refuses",
			doc:     redDocOf(e2e + "|failure"),
			base:    base(nil, map[string]string{e2e: github.CheckPass}),
			refused: "e2e-test refuses: green on the head 0123456 of main: this pull request broke it",
		},
		{
			name:    "required red refuses",
			doc:     redDocOf(build + "|failure|required"),
			base:    base(nil, map[string]string{build: github.CheckFail}),
			refused: build + " refuses: required by main",
		},
		{
			name:    "red required by the base's protection refuses although the document says otherwise",
			doc:     redDocOf(build + "|failure"),
			base:    base([]string{build}, map[string]string{build: github.CheckFail}),
			refused: build + " refuses: required by main",
		},
		{
			name:    "security scan red on both heads refuses",
			doc:     redDocOf("ci/circleci: go-nancy|failure"),
			base:    base(nil, map[string]string{"ci/circleci: go-nancy": github.CheckFail}),
			refused: "ci/circleci: go-nancy refuses: a security scan",
		},
		{
			name:    "base result missing refuses",
			doc:     redDocOf(e2e + "|failure"),
			base:    base(nil, map[string]string{}),
			refused: "e2e-test refuses: no result on the head 0123456 of main",
		},
		{
			name:    "base still running refuses",
			doc:     redDocOf(e2e + "|timed_out"),
			base:    base(nil, map[string]string{e2e: github.CheckPending}),
			refused: "e2e-test refuses: still running on the head 0123456 of main",
		},
		{
			name:     "one let through, one broken: refuses, naming both",
			doc:      redDocOf(e2e+"|failure", "lint|failure"),
			base:     base(nil, map[string]string{e2e: github.CheckFail, "lint": github.CheckPass}),
			accepted: []string{e2e},
			refused:  "lint refuses: green on the head 0123456 of main: this pull request broke it",
		},
		{
			name:    "a red check that died on infrastructure refuses as such, not as the pull request's break",
			doc:     redDocOf("lint|failure"),
			base:    base(nil, map[string]string{"lint": github.CheckPass}),
			infra:   []Infra{{Check: "lint", Repo: "o/r", Run: 7, Job: 9, Signature: "Go module proxy", Line: "stream error"}},
			refused: "lint refuses: a transient infrastructure failure, not this pull request's breakage (Go module proxy: \"stream error\"): rerun the job (`gh run rerun 7 --job 9 --repo o/r`), then the same merge again",
		},
		{
			name:     "a let-through check stays let through whatever its log says",
			doc:      redDocOf(e2e + "|failure"),
			base:     base(nil, map[string]string{e2e: github.CheckFail}),
			infra:    []Infra{{Check: e2e, Repo: "o/r", Run: 7, Job: 9, Signature: "network", Line: "x"}},
			through:  true,
			accepted: []string{e2e},
		},
		{
			name:    "a red CircleCI workflow refuses",
			doc:     []byte(`{"checks":[],"circleci":{"workflows":[{"name":"build","status":"failed"}]}}`),
			base:    base(nil, map[string]string{"build": github.CheckFail}),
			refused: "circleci workflow build refuses: a red CircleCI workflow",
		},
		{
			name:    "no document refuses",
			doc:     []byte("not json"),
			refused: "devctl's document does not read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := JudgeRed(tc.doc, tc.base, tc.infra)
			if j.LetThrough() != tc.through {
				t.Errorf("let through %v, want %v: %s", j.LetThrough(), tc.through, j)
			}
			if !slices.Equal(j.Names(), tc.accepted) && len(j.Names())+len(tc.accepted) > 0 {
				t.Errorf("accepted %q, want %q", j.Names(), tc.accepted)
			}
			if tc.refused != "" && !slices.Contains(j.Refused, tc.refused) {
				t.Errorf("refused %q, want it to carry %q", j.Refused, tc.refused)
			}
			if tc.refused == "" && len(j.Refused) > 0 {
				t.Errorf("refused %q, want none", j.Refused)
			}
		})
	}
}

func TestJudgeRedSaysWhy(t *testing.T) {
	j := JudgeRed(redDocOf(e2e+"|failure"), github.BaseChecks{Base: "main", Head: "abcdef0123", Outcomes: map[string]string{e2e: github.CheckFail}}, nil)
	want := "e2e-test let through: not required, and red on the head abcdef0 of main too: not this pull request's breakage"
	if j.String() != want {
		t.Errorf("judgement %q, want %q", j, want)
	}
}

func TestSecurityScan(t *testing.T) {
	for name, want := range map[string]bool{
		"ci/circleci: go-nancy": true, "gitleaks": true, "govulncheck": true, "Trivy scan": true, "CodeQL": true,
		e2e: false, "ci/circleci: go-build": false, "helm-e2e": false,
	} {
		if SecurityScan(name) != want {
			t.Errorf("SecurityScan(%q) = %v, want %v", name, !want, want)
		}
	}
}

func TestAcceptRed(t *testing.T) {
	got := AcceptRed([]string{Tool, "pr", mergeVerb, failedRepo, "7", "--rebase"}, []string{e2e, "ci/circleci: helm e2e"})
	want := []string{Tool, "pr", mergeVerb, AcceptRedFlag, e2e, AcceptRedFlag, "ci/circleci: helm e2e", failedRepo, "7", "--rebase"}
	if !slices.Equal(got, want) {
		t.Errorf("AcceptRed = %q, want %q", got, want)
	}
	if argv := []string{Tool, "pr", mergeVerb, failedRepo, "7"}; !slices.Equal(AcceptRed(argv, nil), argv) {
		t.Error("AcceptRed without checks changes argv")
	}
}

func TestRed(t *testing.T) {
	for doc, want := range map[string]bool{
		`{"exitCode":1,"verdict":"red"}`:     true,
		`{"exitCode":2,"verdict":"timeout"}`: false,
		`{"exitCode":0,"verdict":"green"}`:   false,
		`not json`:                           false,
	} {
		if Red([]byte(doc)) != want {
			t.Errorf("Red(%s) = %v, want %v", doc, !want, want)
		}
	}
}
