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

// Connector discovery needs a peer wake: an idle turn alone never gets the
// boundary at which a connector attaches. Keep both sides of the wire protocol
// and the post-ACK escalation in the shipped registration skill.
func TestRegisterAgentConnectorHandshake(t *testing.T) {
	registration := strings.Join(strings.Fields(Skill("register-agent")), " ")
	for _, want := range []string{
		"use `ListAgents` once to choose an idle peer",
		`exactly ` + "`" + `SYN <own session name>: reply "ACK" to me with SendMessage and do nothing else.` + "`",
		"Finish the turn's other work and end the turn",
		"do not keep it open with a background wait",
		"starts with `SYN <name>:`, reply exactly `ACK` to that name with `SendMessage` and do nothing else. End the turn.",
		"In the turn the ACK opened, look for the connector tool again. Only if it is still missing in that turn, ask the person to switch it on through the normal `note add` route.",
		"Never hand the work back to the person because a connector tool is missing.",
	} {
		if !strings.Contains(registration, want) {
			t.Errorf("register-agent is missing connector handshake instruction %q", want)
		}
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

// The long-proofs skill says where an agentlab proof runs: outside beekeeper run,
// whose build slot it would hold while it only waits.
func TestLongProofsKeepAgentlabProofsOutOfTheSlot(t *testing.T) {
	if !strings.Contains(strings.Join(strings.Fields(Skill("long-proofs")), " "), "An agentlab proof only waits: never under `beekeeper run`") {
		t.Error("the long-proofs skill does not say an agentlab proof never runs under beekeeper run")
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

// Keep room for new worker reminders without increasing the topic budget.
func TestGitHubSkillHeadroom(t *testing.T) {
	const budget, headroom = 1890, 150
	if n := len(SkillFile("github")); n > budget-headroom {
		t.Errorf("github skill: %d bytes, want at least %d bytes free under %d", n, headroom, budget)
	}
}

// The hook takes a slot for a source scan, so the rule says the scan waits.
func TestGitHubSkillNamesTheScanWait(t *testing.T) {
	if !strings.Contains(Skill("github"), "**Builds/tests/lints/scans**: `beekeeper run`") {
		t.Error("the github skill must say a code scan waits for a build slot")
	}
}

func TestGitHubSkillPersonalAccountGraphQL(t *testing.T) {
	if !strings.Contains(Skill("github"), "**Personal-account GraphQL:** `GH_REPO=<owner/repo> gh api graphql`") {
		t.Error("the github skill must name GH_REPO for personal-account GraphQL")
	}
}

// The on-demand reference must ship with the skill and retain its guidance.
func TestGitHubSkillBrowserAndSecurityReference(t *testing.T) {
	const reference = "references/browser-and-security.md"
	if !strings.Contains(Skill("github"), "before either, read ["+reference+"]("+reference+")") {
		t.Fatal("the github skill must load the reference before browser or Dependabot work")
	}
	raw, err := Skills.ReadFile(SkillsDir + "/github/" + reference)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		"before/after remediation, `beekeeper security alert <owner/repo> <number>`",
		"Unreadable or dismissed is not fixed",
		"App: Dependabot alerts read, accepted on installation",
		"No personal scopes or security-scan bypass",
		"headless: CLI Claude in Chrome tools; desktop: `beekeeper browse",
		"Never wait on site approval",
		"Own App grants: `beekeeper app allow` first; refusals are barriers",
		"Sign-in: `muster auth login --no-browser`, the printed URL in a browse step",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("github reference lacks %q", want)
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
	if !strings.Contains(merge, `--squash --match-head-commit <reviewed-head-sha> --subject "<PR title> (#<n>)"`) {
		t.Error("the merge skill does not name --subject for a personal account's merge")
	}
}

// A code scan's finding is raised on main and names where it was raised: a
// scan of a stale clone reports what main fixed long ago.
func TestCodeScanFindingsNameTheirCommit(t *testing.T) {
	words := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	gh := words(Skill("github"))
	for _, want := range []string{"fresh worktree of `origin/main`, never a shared clone", "names the commit and module versions"} {
		if !strings.Contains(gh, want) {
			t.Errorf("the github skill does not say %q", want)
		}
	}
}

// Multi-repository searches must avoid exhausting GitHub's code-search bucket.
func TestGitHubSkillRoutesMultiRepositoryCodeSearchToClones(t *testing.T) {
	text := strings.Join(strings.Fields(Skill("github")), " ")
	for _, want := range []string{
		"10 requests per minute",
		"Across more than a few repos",
		"`git clone --depth 1`",
		"`<stateDir>/code-scans/<owner>/<repo>` and `rg`",
		"never per-repo `gh search code` loops",
		"configured state directory",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the github skill does not say %q", want)
		}
	}
}

// Shipped merge commands preserve the reviewed head instead of updating it.
func TestGatedMergeLinesPreserveTheHead(t *testing.T) {
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
			if strings.Contains(m, "--update-branch") {
				t.Errorf("%s shows head-mutating command %s", path, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	merge := strings.Join(strings.Fields(Skill("merge")), " ")
	for _, want := range []string{
		"`devctl pr merge <owner/repo> <n>`",
		"`--rebase`",
		"report the repository protection requirement",
		"Never update the branch to retry",
	} {
		if !strings.Contains(merge, want) {
			t.Errorf("the merge skill does not say %q", want)
		}
	}
}

// The role skill loads only from a prompt that opens with its bare slash
// command (guide handover --prompt, a successor's first prompt, the
// person), so a worker's skill listing never carries it.
func TestRoleSkillsAreHiddenFromTheModel(t *testing.T) {
	for _, name := range []string{"guide"} {
		front, _, _ := strings.Cut(strings.TrimPrefix(string(SkillFile(name)), "---\n"), "\n---\n")
		if !slices.Contains(strings.Split(front, "\n"), "disable-model-invocation: true") {
			t.Errorf("%s/SKILL.md lacks disable-model-invocation: true:\n%s", name, front)
		}
	}
}

// Both instructions keep green heads unchanged and require merge-result checks.
func TestWorkerRulesAndMergeSkillPreserveTheBehindHead(t *testing.T) {
	for name, text := range map[string]string{"the worker rules": WorkerRules(), "the merge skill": Skill("merge")} {
		words := strings.Join(strings.Fields(text), " ")
		for _, want := range []string{"merge-result check", "green head"} {
			if !strings.Contains(words, want) {
				t.Errorf("%s do not say %q", name, want)
			}
		}
		if strings.Contains(words, "a behind branch merges its base and pushes") {
			t.Errorf("%s still require a catch-up push", name)
		}
	}
}

// The worker rules and worker topic skills name the harness-neutral CLI
// routes (agents idle --done --report, agents park, note add, the inbox) and
// say nothing of Claude Code's SendMessage or AskUserQuestion tools.
func TestWorkerRulesAndTopicsUseHarnessNeutralRoutes(t *testing.T) {
	rules := strings.Join(strings.Fields(WorkerRules()), " ")
	for _, want := range []string{
		"agents idle --done --report",
		"agents park",
		"note add",
		"inbox",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("the worker rules do not name %q", want)
		}
	}
	for _, name := range append([]string{"worker-rules"}, Topics...) {
		text := Skill(name)
		for _, tool := range []string{"SendMessage", "AskUserQuestion"} {
			if strings.Contains(text, tool) {
				t.Errorf("%s skill names %s", name, tool)
			}
		}
	}
}

func TestWorkerRulesStateFullURLRuleOnce(t *testing.T) {
	rules := strings.Join(strings.Fields(WorkerRules()), " ")
	const want = "Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`."
	if n := strings.Count(rules, want); n != 1 {
		t.Errorf("the worker rules state %q %d times, want 1", want, n)
	}
}

// https://github.com/giantswarm/beekeeper/issues/1795: the guide asks each
// waiting decision in the conversation the person reads; no skill assumes
// a phone or a push notification reaches them.
func TestTheGuideAsksWaitingDecisionsInItsConversation(t *testing.T) {
	err := fs.WalkDir(Skills, SkillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(Skills, path)
		if err != nil {
			return err
		}
		if lower := strings.ToLower(string(b)); strings.Contains(lower, "pushnotification") || strings.Contains(lower, "phone") {
			t.Errorf("%s assumes a phone or a push notification reaches the person", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	guide := strings.Join(strings.Fields(Skill("guide")), " ")
	if !strings.Contains(guide, "ask it in the conversation") || !strings.Contains(guide, "without waiting for them to ask") {
		t.Error("the guide skill does not ask waiting decisions in its conversation")
	}
}

// Topic procedures stay out of the core prompt, leaving room for shared rules.
func TestWorkerRulesLeaveTopicHeadroom(t *testing.T) {
	const maxCoreBytes = 2500
	if n := len(WorkerRules()); n > maxCoreBytes {
		t.Errorf("worker rules: %d bytes, want at most %d for headroom", n, maxCoreBytes)
	}
	for _, procedure := range []string{"alerts snapshot", "systemd-run", "agentlab proof"} {
		if strings.Contains(WorkerRules(), procedure) {
			t.Errorf("topic procedure %q remains in worker rules", procedure)
		}
	}
}

func TestOperationalTopicRules(t *testing.T) {
	for topic, rules := range map[string][]string{
		"alerts":      {"KUBECONFIG=", "beekeeper alerts snapshot", "never Alertmanager/Mimir"},
		"long-proofs": {"10 minutes", "slot wait", "systemd-run --user", "beekeeper timer add", "--probe", "--wake", "RemainAfterExit=yes", "SubState", "exited|failed", "ExecMainStatus"},
	} {
		text := strings.Join(strings.Fields(Skill(topic)), " ")
		for _, rule := range rules {
			if !strings.Contains(text, rule) {
				t.Errorf("%s skill lacks %q", topic, rule)
			}
		}
	}
}
