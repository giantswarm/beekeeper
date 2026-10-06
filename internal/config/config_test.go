package config

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != "/state/beekeeper" || c.LeaseDir != "/state/beekeeper/leases" || c.GitHub.Floor != 2500 ||
		c.Watch.Interval.Duration != 30*time.Second || c.Watch.LoadMax != 0 || c.Watch.LoadLimit(24) != 36 || c.Supervisor.RelayAt != 400_000 || c.Guide.RelayAt != 150_000 {
		t.Errorf("defaults = %+v", c)
	}
	if len(c.Notify.Kinds) != 5 || c.Notify.Policy().Quiet != nil {
		t.Errorf("notify defaults = %+v", c.Notify)
	}
	if al := c.Alerts; al.PageSeverity != "page" || al.OwnerGrace.Duration != 15*time.Minute {
		t.Errorf("page owner defaults = %q %s", al.PageSeverity, al.OwnerGrace)
	}
	if w := c.Watch; w.ToolProcsMax != 1000 || len(w.Tools) != 6 || c.Desktop.TypingQuiet.Duration != 30*time.Second {
		t.Errorf("LOAD and typing defaults = %d %v %s", w.ToolProcsMax, w.Tools, c.Desktop.TypingQuiet)
	}
	if len(c.Alerts.Quiet) != 0 || !slices.Equal(c.Alerts.Ignore, DefaultIgnore) || c.Alerts.Tenant != "" {
		t.Errorf("alerts without a team = %+v, want no quiet rule, Watchdog ignored, no tenant", c.Alerts)
	}
	if c.Kube != (Kube{}) || c.Kube.Context("alpha") != "" || len(c.Lanes) != 0 || len(c.Merge.DevctlOwners) != 0 ||
		c.Guide.Skill != "" || c.Guide.Person != "" || c.Supervisor.Skill != "" || c.Ollama.URL != "" || c.Lemonade.URL != "" {
		t.Errorf("organisation and desk defaults are set: %+v", c)
	}
	if !c.IsLeasable(Browser) || c.IsLeasable("kind-1") {
		t.Error("only the browser is leasable without resources")
	}
}

func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := `resources: [kind-1, staging]
grantTTL: 10m
github: {floor: 3000}
watch: {interval: 1m}
alerts:
  installations: [alpha, {name: beta, context: admin@beta, floor: warning}]
  team: bumblebee
  flap: {changes: 3}
notify:
  kinds: [due, budget]
  quietHours: "22:00-07:00"
  urgency: {due: critical}
supervisor: {relayAt: 1.5M}
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLeasable("staging") || c.GrantTTL.Duration != 10*time.Minute || c.GitHub.Floor != 3000 ||
		c.Watch.Interval.Duration != time.Minute || c.Supervisor.RelayAt != 1_500_000 {
		t.Errorf("config = %+v", c)
	}
	if n := c.Notify; len(n.Kinds) != 2 || n.Repeat.Duration != 30*time.Minute || n.Policy().Quiet == nil || n.Urgency["due"] != "critical" {
		t.Errorf("notify = %+v", n)
	}
	al := c.Alerts
	if len(al.Installations) != 2 || al.Installations[0] != (Installation{Name: "alpha"}) ||
		al.Installations[1] != (Installation{Name: "beta", Context: "admin@beta", Floor: "warning"}) {
		t.Errorf("installations = %+v", al.Installations)
	}
	if len(al.Ignore) != 1 || al.Collapse != 3 || al.Every.Duration != 5*time.Minute || al.Timeout.Duration != time.Minute ||
		al.Flap.Changes != 3 || al.Flap.Window.Duration != time.Hour {
		t.Errorf("alerts defaults = %+v", al)
	}
	if want := []alerts.Quiet{OtherTeamsNotify}; !slices.Equal(al.Quiet, want) ||
		!slices.Equal(c.Watch.QuietSessions, DefaultQuietSessions) {
		t.Errorf("quiet defaults = %+v, %q", al.Quiet, c.Watch.QuietSessions)
	}
}

func TestQuietRulesReplaceTheDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := `alerts: {quiet: [{installation: alpha, alertname: "Karpenter*"}]}
watch: {quietSessions: []}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []alerts.Quiet{{Installation: "alpha", Alertname: "Karpenter*"}}; !slices.Equal(c.Alerts.Quiet, want) {
		t.Errorf("quiet = %+v, want %+v", c.Alerts.Quiet, want)
	}
	if len(c.Watch.QuietSessions) != 0 {
		t.Errorf("quietSessions = %q, want none", c.Watch.QuietSessions)
	}
}

func TestLabs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("resources: [agentlab-1, agentlab-2, staging]\nlabs: {agentlab-1: agentlab, agentlab-2: agentlab-2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.LabCluster("agentlab-1") != "agentlab" || c.LabCluster("staging") != "" {
		t.Errorf("lab clusters = %v", c.Labs)
	}
	if c.LabLease("agentlab-2") != "agentlab-2" || c.LabLease("kind") != "" {
		t.Errorf("lab leases = %v", c.Labs)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"browser as resource":  "resources: [browser]",
		"path as resource":     "resources: [../x]",
		"lab not a resource":   "resources: [kind-1]\nlabs: {kind-2: kind-2}",
		"lab without cluster":  "resources: [kind-1]\nlabs: {kind-1: \"\"}",
		"lab cluster twice":    "resources: [kind-1, kind-2]\nlabs: {kind-1: kind, kind-2: kind}",
		"nameless install":     "alerts: {installations: [{context: x}]}",
		"unknown floor":        "alerts: {installations: [{name: x, floor: low}]}",
		"unknown pageSeverity": "alerts: {pageSeverity: urgent}",
		"short ownerGrace":     "alerts: {ownerGrace: 10s}",
		"flapping at once":     "alerts: {flap: {changes: 1}}",
		"bad duration":         "grantTTL: soon",
		"skill and file":       "supervisor: {skill: supervise, instructions: /x.md}",
		"unknown notify kind":  "notify: {kinds: [due, alerts]}",
		"machine notify kind":  "notify: {kinds: [due, oom-line]}",
		"machine urgency":      "notify: {urgency: {oom-kill: critical}}",
		"unknown urgency":      "notify: {urgency: {due: urgent}}",
		"urgency of no kind":   "notify: {urgency: {sessions: low}}",
		"bad quiet hours":      "notify: {quietHours: 22-7}",
		"bad relayAt":          "supervisor: {relayAt: 400kb}",
		"negative relayAt":     "supervisor: {relayAt: -1}",
		"bad store glob":       "outbound: {storeDeny: [{vault: \"[\"}]}",
		"bad outbound path":    "outbound: {paths: [\"[\"]}",
		"age without match":    "secret: {ageIdentities: [{ref: op://V/i/f}]}",
		"age bad recipient":    "secret: {ageIdentities: [{recipient: ssh-ed25519, ref: op://V/i/f}]}",
		"age without op ref":   "secret: {ageIdentities: [{recipient: age1x, ref: ~/key.txt}]}",
		"age relative file":    "secret: {ageIdentities: [{recipient: age1x, ref: \"file://key.txt\"}]}",
		"age bad pathRegex":    "secret: {ageIdentities: [{pathRegex: \"[\", ref: op://V/i/f}]}",
		"nameless board step":  "board: {order: [{status: [backlog]}]}",
		"search with fields":   "board: {order: [{name: q, search: \"repo:o/r\", status: [backlog]}]}",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAgeIdentityRefs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := "secret: {ageIdentities: [{recipient: age1x, ref: op://V/i/f}, {pathRegex: /repo/, ref: \"file:///home/me/keys.txt\"}]}"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Secret.AgeIdentities[1].Ref; got != "file:///home/me/keys.txt" {
		t.Errorf("file ref = %q", got)
	}
}

func TestParseTokens(t *testing.T) {
	for in, want := range map[string]Tokens{"400k": 400_000, "400K": 400_000, "1m": 1_000_000, "0.5M": 500_000, "250000": 250_000, " 80k ": 80_000} {
		if got, err := ParseTokens(in); err != nil || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "k", "0", "-5k", "lots", "4e", "inf", "NaN", "0.5"} {
		if _, err := ParseTokens(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

const (
	backstage = "giantswarm/backstage"
	portal    = "portal-tools"
)

func TestLanes(t *testing.T) {
	c := &Config{Lanes: []Lane{
		{Name: portal, Repositories: []string{backstage, "marge"}, Installation: "gazelle"},
		{Name: "serving", Repositories: []string{"giantswarm/model-manager"}},
	}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for repo, lane := range map[string]string{
		backstage:                  portal,
		"giantswarm/marge":         portal,
		"giantswarm/model-manager": "serving",
		"giantswarm/devctl":        "giantswarm/devctl",
	} {
		if got := c.LaneOf(repo).Name; got != lane {
			t.Errorf("%s: lane %s, want %s", repo, got, lane)
		}
	}
	c.Lanes = append(c.Lanes, Lane{Name: "dup", Repositories: []string{backstage}})
	if err := c.validate(); err == nil {
		t.Error("a repository in two lanes validates")
	}
}

func TestMetricsModels(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := "metrics:\n  models:\n    claude-opus-5-5: {input: 1, output: 2, contextWindow: 500000}\n    my-model: {input: 3}\n  runaway: {sameErrorRepeats: -1}\n"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Metrics
	if got, _ := m.Model("claude-opus-5-5"); got.Input != 1 || got.ContextWindow != 500000 {
		t.Errorf("a configured model replaces the default: %+v", got)
	}
	if got, ok := m.Model("claude-haiku-4-5-20251001"); !ok || got.Input != 1 {
		t.Errorf("a dated snapshot is priced like its model: %+v %v", got, ok)
	}
	for _, unknown := range []string{"claude-opus-5-6", "claude-opus-5-5-fast", "claude"} {
		if _, ok := m.Model(unknown); ok {
			t.Errorf("%s has no price", unknown)
		}
	}
	if _, ok := m.Model("my-model"); !ok {
		t.Error("a configured model is priced")
	}
	if r := m.Runaway; r.SameErrorRepeats != -1 || r.GitHubCallsPerHour != 1000 || r.ContextFill != 0.9 {
		t.Errorf("runaway %+v", r)
	}
}

func TestLoadReporter(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(raw string) string {
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := Load(write("guide: {person: Ada}\nreporter: {every: 1h, brief: ~/brief.md, model: sonnet}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Reporter
	if !r.Enabled() || r.Every.Duration != time.Hour || r.Brief != filepath.Join(home, "brief.md") || r.Person != "Ada" ||
		r.Dir != home || r.Timeout.Duration != 20*time.Minute || r.Model != "sonnet" {
		t.Errorf("reporter = %+v", r)
	}
	if c, err := Load(write("")); err != nil || c.Reporter.Enabled() {
		t.Errorf("no reporter section: %+v, %v", c.Reporter, err)
	}
	if c, err := Load(write("reporter: {every: 1h, brief: b.md, tz: Europe/Athens, reviews: [example/plans]}\n")); err != nil || c.Reporter.TZ != "Europe/Athens" || c.Reporter.Reviews[0] != "example/plans" {
		t.Errorf("tz and reviews: %+v, %v", c.Reporter, err)
	}
	if c, err := Load(write("plans: {repositories: [example/plans]}\n")); err != nil || !c.Plans.Covers("Example/Plans") || c.Plans.Covers("example/other") || c.Plans.Check != DefaultPlansCheck {
		t.Errorf("plans: %+v, %v", c.Plans, err)
	}
	for _, raw := range []string{"reporter: {every: 1h}\n", "reporter: {every: 10s, brief: b.md}\n",
		"reporter: {every: 1h, brief: b.md, tz: Mars/Olympus}\n", "reporter: {every: 1h, brief: b.md, reviews: [plans]}\n", "plans: {repositories: [plans]}\n"} {
		if _, err := Load(write(raw)); err == nil {
			t.Errorf("%q loads", raw)
		}
	}
}

// devctl serves the owners of merge.devctlOwners, none by default; any
// other owner's repository takes the plain squash merge.
func TestDevctlServes(t *testing.T) {
	var c Config
	if err := c.defaults(); err != nil {
		t.Fatal(err)
	}
	if c.Merge.DevctlServes("example/x") {
		t.Error("devctl serves an owner by default")
	}
	c.Merge.DevctlOwners = []string{"example"}
	for repo, want := range map[string]bool{"example/beekeeper": true, "Example/x": true, "someone/notebook": false} {
		if got := c.Merge.DevctlServes(repo); got != want {
			t.Errorf("%s: %v, want %v", repo, got, want)
		}
	}
}

func TestModelServerLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("resources: [agentlab-1]\nollama: {url: http://localhost:11434}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Leasable(), ","); got != "agentlab-1,model-server,browser" {
		t.Errorf("leasable %s", got)
	}
	if o := c.Ollama; o.BudgetGiB != 12 || o.MaxBudgetGiB != 24 || strings.Join(o.LabTests, ",") != "models-test" {
		t.Errorf("model server defaults %+v", o)
	}
	if c.Lemonade.URL != "" {
		t.Errorf("a machine without Lemonade configured watches one at %s", c.Lemonade.URL)
	}
	for _, bad := range []string{"resources: [model-server]\n", "ollama: {url: http://localhost:11434, budgetGiB: 30}\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%q loads", bad)
		}
	}
}

// The example configuration loads and sets what earlier releases compiled
// in: the kube guard on, absolute thresholds, the rewrite shell, two kind
// labs, the role skills, the ignored and quiet alerts, devctl's owner.
func TestExampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "docs", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kube.Production != "production" || c.Kube.Context("staging") != "login.example.com-staging" ||
		c.Shell != "zsh" || c.KindClusters(1<<30) != 2 || c.MemcapMax(1<<30) != "12G" {
		t.Errorf("kube %+v, shell %q, kind %d, memcap %s", c.Kube, c.Shell, c.KindClusters(1<<30), c.MemcapMax(1<<30))
	}
	w := c.Watch
	for got, want := range map[int]int{w.AvailMin(1): 10240, w.SwapMax(1): 10000, w.OOMDHeadroomMin(1): 1024,
		w.ScopeAnonMax(1): 28000, w.GTTMax(1): 24576, w.TmpMax(1): 20000, w.DiskMin(1): 102400} {
		if got != want {
			t.Errorf("threshold %d, want %d", got, want)
		}
	}
	if c.Supervisor.Skill != "beekeeper:supervise" || c.Guide.Skill != "beekeeper:guide" || c.Guide.Person != "Ada" ||
		c.GitHub.ProbeRepo != "example-org/tools" || !c.Merge.DevctlServes("giantswarm/x") || c.Ollama.URL == "" ||
		c.Alerts.Tenant != "example-org" || len(c.Alerts.Installations) != 3 || len(c.Lanes) != 2 ||
		c.Board.Project != 12 || len(c.Board.Order) != 8 || !c.Board.Order[1].SubIssues || c.Board.StaleAfter.Hours() != 8760 {
		t.Errorf("config = %+v", c)
	}
	if tp := c.Teleport; !tp.Enabled() || tp.Proxy != "login.example.com:443" || tp.Auth != "github" || tp.Tsh != "tsh" ||
		tp.RenewBefore.Duration != 90*time.Minute || tp.WarnBefore.Duration != time.Hour || tp.Every.Duration != 10*time.Minute {
		t.Errorf("teleport %+v", tp)
	}
	if want := []alerts.Quiet{{Cluster: "t-*"}, OtherTeamsNotify}; !slices.Equal(c.Alerts.Quiet, want) ||
		!slices.Contains(c.Alerts.Ignore, "InhibitionOutsideWorkingHours") {
		t.Errorf("alerts quiet %+v, ignore %q", c.Alerts.Quiet, c.Alerts.Ignore)
	}
}

// Unset, the memory thresholds are fractions of what the machine has, and
// an unknown total never fires.
func TestThresholdsDefaultToFractions(t *testing.T) {
	var c Config
	if err := c.defaults(); err != nil {
		t.Fatal(err)
	}
	w := c.Watch
	if w.AvailMin(100_000) != 12_000 || w.ScopeAnonMax(100_000) != 32_000 || w.SwapMax(10_000) != 6_000 ||
		w.DiskMin(1_000_000) != 50_000 || w.TmpMax(0) != math.MaxInt || w.AvailMin(0) != 0 {
		t.Errorf("fractions: avail %d scope %d swap %d disk %d", w.AvailMin(100_000), w.ScopeAnonMax(100_000), w.SwapMax(10_000), w.DiskMin(1_000_000))
	}
	if c.KindClusters(88_000) != 2 || c.KindClusters(16_000) != 1 || c.MemcapMax(100_000) != "14000M" || c.MemcapMax(0) != "infinity" {
		t.Errorf("kind %d/%d, memcap %s", c.KindClusters(88_000), c.KindClusters(16_000), c.MemcapMax(100_000))
	}
	if _, err := Load(writeTemp(t, "kube: {contextTemplate: login.example.com}\n")); err == nil {
		t.Error("a context template without {installation} loads")
	}
}

func writeTemp(t *testing.T, raw string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOutbound(t *testing.T) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("outbound: {phrases: [x], paths: [~/plans, /srv/*.md], storeDeny: [{vault: Private}]}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Outbound
	if o.Paths[0] != filepath.Join(home, "plans") || o.Paths[1] != "/srv/*.md" || o.StoreDeny[0].Vault != "Private" {
		t.Errorf("outbound %+v", o)
	}
	if len(o.SweepRoots) != 1 || o.SweepRoots[0] != home || o.SweepDepth != 5 {
		t.Errorf("sweep defaults: roots %q depth %d, want the home directory 5 deep", o.SweepRoots, o.SweepDepth)
	}
}

func TestAgentShell(t *testing.T) {
	c := &Config{}
	if err := c.defaults(); err != nil {
		t.Fatal(err)
	}
	if sh := c.Agents.Shell; !slices.Equal(sh.Unalias, DefaultUnalias) || sh.Globs != GlobsLiteral {
		t.Errorf("defaults: %+v", sh)
	}
	for _, bad := range []AgentShell{{Globs: "nullglob"}, {Unalias: []string{"grep; rm -rf ~"}}} {
		if err := (&Config{Agents: Agents{Shell: bad}}).validate(); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
	none := &Config{Agents: Agents{Shell: AgentShell{Unalias: []string{}, Globs: GlobsShell}}}
	if err := none.defaults(); err != nil || len(none.Agents.Shell.Unalias) != 0 || none.Agents.Shell.Globs != GlobsShell {
		t.Errorf("an empty list and shell stay: %+v %v", none.Agents.Shell, err)
	}
}

func TestSandbox(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	home, _ := os.UserHomeDir()
	c, err := Load(write("sandbox: {allowRead: [~/projects], allowWrite: [/var/tmp/x]}\n"))
	if err != nil || c.Sandbox.AllowRead[0] != filepath.Join(home, "projects") || c.Sandbox.AllowWrite[0] != "/var/tmp/x" {
		t.Fatalf("%+v, %v", c.Sandbox, err)
	}
	if c.Sandbox.ProxyPort != 3190 {
		t.Errorf("default proxyPort = %d", c.Sandbox.ProxyPort)
	}
	for _, bad := range []string{"sandbox: {allowRead: [projects]}\n", "sandbox: {proxyPort: 70000}\n", "sandbox: {proxyPort: -1}\n",
		"sandbox: {proxyPort: 3191, domains: [\"127.0.0.1:3191\"]}\n"} {
		if _, err := Load(write(bad)); err == nil {
			t.Errorf("%q loads", bad)
		}
	}
	// loopback only by a lab's port: a bare loopback host opens every
	// listener on the host through the sandbox proxy
	for _, d := range []string{"127.0.0.1", "localhost", "127.0.0.1:*", "[::1]", "::1", "*.localhost", "127.0.0.2", "*"} {
		if _, err := Load(write("sandbox: {domains: ['" + d + "']}\n")); err == nil || !strings.Contains(err.Error(), "127.0.0.1:<port>") {
			t.Errorf("domain %q: %v", d, err)
		}
	}
	if _, err := Load(write("sandbox: {domains: ['127.0.0.1:6443', '[::1]:6443', 'github.com', '*.circleci.com']}\n")); err != nil {
		t.Errorf("a lab's port: %v", err)
	}
}
