package plugin

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/brieflint"
)

// The shipped skills pass the lint the desks run on theirs: this test is
// the project's CI step for it.
func TestShippedSkillsPassTheLint(t *testing.T) {
	found, err := brieflint.LintFS(Skills, SkillsDir, "plugin")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s:%d: %s: %s", f.Path, f.Line, f.Rule, f.Text)
	}
}

func TestWorkerRulesIsTheSkillBody(t *testing.T) {
	r := WorkerRules()
	if strings.HasPrefix(r, "---") || strings.Contains(r, "\nname: worker-rules") {
		t.Errorf("frontmatter left in:\n%.200s", r)
	}
	if !strings.HasPrefix(r, "# worker-rules") || !strings.Contains(r, "the supervisor") {
		t.Errorf("not the worker-rules skill:\n%.200s", r)
	}
}

// The worker rules name every topic skill, each topic is a skill of its
// own name, and none restates the rules' heading.
func TestWorkerRulesNameTheTopics(t *testing.T) {
	r := WorkerRules()
	for _, topic := range Topics {
		raw := string(SkillFile(topic))
		if !strings.Contains(r, "`beekeeper:"+topic+"`") {
			t.Errorf("the worker rules do not name beekeeper:%s", topic)
		}
		if !strings.HasPrefix(raw, "---\nname: "+topic+"\n") || !strings.HasPrefix(Skill(topic), "# "+topic+" — ") {
			t.Errorf("%s/SKILL.md is not the %s skill:\n%.200s", topic, topic, raw)
		}
	}
}

// The worker rules say where an agentlab proof runs: outside beekeeper run,
// whose build slot it would hold while it only waits.
func TestWorkerRulesKeepAgentlabProofsOutOfTheSlot(t *testing.T) {
	if !strings.Contains(strings.Join(strings.Fields(WorkerRules()), " "), "an agentlab proof only waits: never under `beekeeper run`") {
		t.Error("the worker rules do not say an agentlab proof never runs under beekeeper run")
	}
}

// The budgets a worker's context keeps, in bytes at the measured 2.7 bytes
// per token of this text: the core rules at most 1,000 tokens, a topic
// skill at most 700, its description at most 60.
func TestWorkerRulesBudget(t *testing.T) {
	rules := "plugin/" + SkillsDir + "/worker-rules/SKILL.md"
	if msg := overBudget(rules, "the worker rules", len(WorkerRules()), 1000); msg != "" {
		t.Error(msg)
	}
	for _, topic := range Topics {
		file := "plugin/" + SkillsDir + "/" + topic + "/SKILL.md"
		raw := string(SkillFile(topic))
		if msg := overBudget(file, topic, len(raw), 700); msg != "" {
			t.Error(msg)
		}
		desc, _, _ := strings.Cut(raw[strings.Index(raw, "\ndescription: ")+len("\ndescription: "):], "\n")
		if msg := overBudget(file, topic+"'s description", len(desc), 60); msg != "" {
			t.Error(msg)
		}
	}
}

// An over-budget text fails naming its file, its size and the budget.
func TestOverBudgetNamesFileSizeAndBudget(t *testing.T) {
	const want = "skills/worker-rules/SKILL.md (the worker rules): 2,731 bytes, budget 2,700 (1,000 tokens at 2.7 bytes per token)"
	if got := overBudget("skills/worker-rules/SKILL.md", "the worker rules", 2731, 1000); got != want {
		t.Errorf("overBudget = %q, want %q", got, want)
	}
	if got := overBudget("skills/worker-rules/SKILL.md", "the worker rules", 2700, 1000); got != "" {
		t.Errorf("at the budget: %q, want none", got)
	}
}

// overBudget is the failure for a text of n bytes over its budget of tokens
// at bytesPerToken, naming the file, the size and the budget; "" within it.
func overBudget(file, what string, n, tokens int) string {
	const bytesPerToken = 2.7
	budget := int(float64(tokens) * bytesPerToken)
	if n <= budget {
		return ""
	}
	return fmt.Sprintf("%s (%s): %s bytes, budget %s (%s tokens at %v bytes per token)",
		file, what, thousands(n), thousands(budget), thousands(tokens), bytesPerToken)
}

// thousands is n with a comma between its groups of three digits.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func TestBody(t *testing.T) {
	for in, want := range map[string]string{
		"---\nname: x\n---\n# X\n": "# X\n",
		"# No frontmatter\n":       "# No frontmatter\n",
		"---\nunclosed\n":          "---\nunclosed\n",
	} {
		if got := body(in); got != want {
			t.Errorf("body(%q) = %q, want %q", in, got, want)
		}
	}
}

// No shipped skill suggests a lease wait longer than a foreground command
// runs: a background wait of hours does not outlive a desktop turn, a
// longer wait is lease claim --park.
func TestSkillsSuggestNoLongLeaseWait(t *testing.T) {
	long := regexp.MustCompile(`--wait [0-9]+h|probe timer on .beekeeper lease status`)
	err := fs.WalkDir(Skills, SkillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := Skills.ReadFile(path)
		if err != nil {
			return err
		}
		if m := long.FindString(string(raw)); m != "" {
			t.Errorf("%s suggests %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := Skills.ReadFile(SkillsDir + "/lease/SKILL.md"); err != nil || !strings.Contains(string(raw), "--park") {
		t.Errorf("the lease skill names no --park: %v", err)
	}
}

// A personal account's repository is merged with gh, outside devctl: the
// squash subject is the PR title only when --subject names it, as gh takes a
// one-commit PR's commit message otherwise.
func TestMergeSkillNamesTheSquashSubject(t *testing.T) {
	merge := strings.Join(strings.Fields(Skill("merge")), " ")
	if !strings.Contains(merge, `--squash --subject "<PR title> (#<n>)"`) {
		t.Error("the merge skill does not name --subject for a personal account's merge")
	}
}

// A code scan's finding is raised on main and names where it was raised,
// and the supervisor dispatches one only once that is the head: a scan of
// a stale clone reports what main fixed long ago.
func TestCodeScanFindingsNameTheirCommit(t *testing.T) {
	words := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	gh := words(Skill("github"))
	for _, want := range []string{"fresh worktree of `origin/main`, never a shared clone", "names the commit and module versions"} {
		if !strings.Contains(gh, want) {
			t.Errorf("the github skill does not say %q", want)
		}
	}
	raw, err := Skills.ReadFile(SkillsDir + "/supervise/references/scheduling.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(words(string(raw)), "a code scan's finding only once the commit it names is the default branch's head") {
		t.Error("the supervisor's scheduling does not check a scan finding's commit against the head")
	}
}

// Every gated merge line a shipped skill shows passes --update-branch: from
// devctl 8.140.0 a head behind its base is exit 3 without it, strict
// protection or not. The merge skill says what an exit 3 that still says
// behind gets: the behind rule.
func TestGatedMergeLinesUpdateTheBranch(t *testing.T) {
	line := regexp.MustCompile("`devctl pr merge [^`]+`")
	err := fs.WalkDir(Skills, SkillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := Skills.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range line.FindAllString(string(raw), -1) {
			if !strings.Contains(m, "--update-branch") {
				t.Errorf("%s shows %s without --update-branch", path, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	merge := strings.Join(strings.Fields(Skill("merge")), " ")
	for _, want := range []string{
		"`devctl pr merge <owner/repo> <n> --update-branch`",
		"`--rebase --update-branch`",
		`3 still "behind": as below, then the same merge again`,
	} {
		if !strings.Contains(merge, want) {
			t.Errorf("the merge skill does not say %q", want)
		}
	}
}

// The role skills load only from a prompt that opens with their bare slash
// command (handover --prompt, a successor's first prompt, the person), so a
// worker's skill listing never carries them.
func TestRoleSkillsAreHiddenFromTheModel(t *testing.T) {
	for _, name := range []string{"supervise", "guide"} {
		front, _, _ := strings.Cut(strings.TrimPrefix(string(SkillFile(name)), "---\n"), "\n---\n")
		if !slices.Contains(strings.Split(front, "\n"), "disable-model-invocation: true") {
			t.Errorf("%s/SKILL.md lacks disable-model-invocation: true:\n%s", name, front)
		}
	}
}

// behindRule is what a branch behind its base gets, per merge method: a
// rebase line's history stays linear, the gate rebasing the branch.
const behindRule = "a behind branch merges its base and pushes, never on a rebase line"

// The worker rules and the merge skill give a branch behind its base the
// same rule, word for word.
func TestWorkerRulesAndMergeSkillShareTheBehindRule(t *testing.T) {
	words := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for name, text := range map[string]string{"the worker rules": WorkerRules(), "the merge skill": Skill("merge")} {
		if !strings.Contains(words(text), behindRule) {
			t.Errorf("%s do not say %q", name, behindRule)
		}
	}
}
