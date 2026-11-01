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
	if w, k := c.Watch, c.Capacity; w.SwapWindow.Duration != 10*time.Minute || k.Floor != DefaultFloor || k.Ceiling != DefaultCeiling || k.ReviewHeadroom != 2 || k.SwapGrowthMaxMiB != 256 || k.SwapPSIMax != 0 || k.AvailMinMiB != 20<<10 {
		t.Errorf("swap guard and capacity defaults = %s %d/%d %d %g %d", w.SwapWindow, k.Floor, k.Ceiling, k.SwapGrowthMaxMiB, k.SwapPSIMax, k.AvailMinMiB)
	}
	if len(c.Alerts.Quiet) != 0 || !slices.Equal(c.Alerts.Ignore, DefaultIgnore) || c.Alerts.Tenant != "" {
		t.Errorf("alerts without a team = %+v, want no quiet rule, Watchdog ignored, no tenant", c.Alerts)
	}
	if c.Kube.Production != "" || c.Kube.ContextTemplate != "" || len(c.Kube.Kubeconfig) != 0 || c.Kube.Context("alpha") != "" || len(c.Lanes) != 0 || len(c.Merge.DevctlOwners) != 0 ||
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

func TestSinkSelectsItsVerbsAllByDefault(t *testing.T) {
	for raw, want := range map[string][]string{
		"":                                       nil,
		"sink: {repo: o/site, workflow: r.yaml}": SinkVerbs,
		"sink: {repo: o/site, workflow: r.yaml, verbs: [plan.ready, merged]}": {SinkPlanReady, SinkMerged},
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, v := range SinkVerbs {
			if c.Sink.Selects(v) {
				got = append(got, v)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%q selects %q, want %q", raw, got, want)
		}
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
		"browser as resource":         "resources: [browser]",
		"path as resource":            "resources: [../x]",
		"lab not a resource":          "resources: [kind-1]\nlabs: {kind-2: kind-2}",
		"lab without cluster":         "resources: [kind-1]\nlabs: {kind-1: \"\"}",
		"lab cluster twice":           "resources: [kind-1, kind-2]\nlabs: {kind-1: kind, kind-2: kind}",
		"nameless install":            "alerts: {installations: [{context: x}]}",
		"unknown floor":               "alerts: {installations: [{name: x, floor: low}]}",
		"unknown pageSeverity":        "alerts: {pageSeverity: urgent}",
		"short ownerGrace":            "alerts: {ownerGrace: 10s}",
		"discover without a team":     "alerts: {discover: {every: 1h}}",
		"bad discover exclude":        "alerts: {team: t, discover: {exclude: ['[']}}",
		"pagerduty without services":  "alerts: {pagerduty: {context: example}}",
		"flapping at once":            "alerts: {flap: {changes: 1}}",
		"bad duration":                "grantTTL: soon",
		"sink without a repo":         "sink: {workflow: refresh.yaml}",
		"sink repo not owner/repo":    "sink: {repo: site, workflow: refresh.yaml}",
		"sink without a workflow":     "sink: {repo: o/site}",
		"sink of an unknown verb":     "sink: {repo: o/site, workflow: refresh.yaml, verbs: [note.add]}",
		"skill and file":              "supervisor: {skill: supervise, instructions: /x.md}",
		"unknown worker profile":      "agents: {profile: lean}",
		"profile name as a path":      "agents: {profiles: {../x: {tools: [Read]}}}",
		"unknown role profile":        "guide: {profile: lean}\nagents: {profiles: {slim: {tools: [Bash]}}}",
		"unknown notify kind":         "notify: {kinds: [due, alerts]}",
		"machine notify kind":         "notify: {kinds: [due, oom-line]}",
		"machine urgency":             "notify: {urgency: {oom-kill: critical}}",
		"unknown urgency":             "notify: {urgency: {due: urgent}}",
		"urgency of no kind":          "notify: {urgency: {sessions: low}}",
		"bad quiet hours":             "notify: {quietHours: 22-7}",
		"bad relayAt":                 "supervisor: {relayAt: 400kb}",
		"team files without a dir":    "supervisor: {ownership: {team: t, teamFiles: o/github}}",
		"bad review pattern":          "supervisor: {ownership: {review: [\"o/[\"]}}",
		"review without an owner":     "supervisor: {ownership: {review: [shared-configs]}}",
		"never without an owner":      "supervisor: {ownership: {never: [tool]}}",
		"never without a name":        "supervisor: {ownership: {never: [o/]}}",
		"negative relayAt":            "supervisor: {relayAt: -1}",
		"bad store glob":              "outbound: {storeDeny: [{vault: \"[\"}]}",
		"bad outbound path":           "outbound: {paths: [\"[\"]}",
		"age without match":           "secret: {ageIdentities: [{ref: op://V/i/f}]}",
		"age bad recipient":           "secret: {ageIdentities: [{recipient: ssh-ed25519, ref: op://V/i/f}]}",
		"age without op ref":          "secret: {ageIdentities: [{recipient: age1x, ref: ~/key.txt}]}",
		"age relative file":           "secret: {ageIdentities: [{recipient: age1x, ref: \"file://key.txt\"}]}",
		"age bad pathRegex":           "secret: {ageIdentities: [{pathRegex: \"[\", ref: op://V/i/f}]}",
		"nameless board step":         "board: {order: [{status: [backlog]}]}",
		"search with fields":          "board: {order: [{name: q, search: \"repo:o/r\", status: [backlog]}]}",
		"omp key as value":            "omp: {providers: {spark: {apiKey: sk-planted}}}",
		"omp key half ref":            "omp: {providers: {spark: {apiKey: op://Vault/item}}}",
		"omp key no ref":              "omp: {providers: {spark: {}}}",
		"swap window under 5m":        "watch: {swapWindow: 2m}",
		"negative swapPSIMax":         "capacity: {swapPSIMax: -1}",
		"negative capacity floor":     "capacity: {floor: -1, ceiling: 0}",
		"negative capacity ceiling":   "capacity: {ceiling: -1}",
		"ceiling under floor":         "capacity: {floor: 5, ceiling: 0}",
		"usage without command":       "capacity: {usage: {highPercent: 90}}",
		"usage bands without command": "capacity: {usage: {fiveHour: [{upTo: 80, ceiling: 4}]}}",
		"usage empty executable":      "capacity: {usage: {command: [\"  \"]}}",
		"usage negative every":        "capacity: {usage: {command: [reader], every: -1m}}",
		"usage negative timeout":      "capacity: {usage: {command: [reader], timeout: -1s}}",
		"usage zero highPercent":      "capacity: {usage: {command: [reader], highPercent: 0}}",
		"usage over-100 highPercent":  "capacity: {usage: {command: [reader], highPercent: 101}}",
		"usage NaN highPercent":       "capacity: {usage: {command: [reader], highPercent: .nan}}",
		"usage band zero upTo":        "capacity: {usage: {command: [reader], fiveHour: [{upTo: 0, ceiling: 4}]}}",
		"usage band over-100 upTo":    "capacity: {usage: {command: [reader], fiveHour: [{upTo: 105, ceiling: 4}]}}",
		"usage band unsorted":         "capacity: {usage: {command: [reader], fiveHour: [{upTo: 80, ceiling: 4}, {upTo: 80, ceiling: 2}]}}",
		"usage band missing ceiling":  "capacity: {usage: {command: [reader], sevenDay: [{upTo: 80, floor: 2}]}}",
		"usage band negative floor":   "capacity: {usage: {command: [reader], sevenDay: [{upTo: 80, floor: -1, ceiling: 2}]}}",
		"usage band ceiling < floor":  "capacity: {usage: {command: [reader], sevenDay: [{upTo: 80, floor: 5, ceiling: 2}]}}",
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

// An omp provider's key is an op:// reference; the models file has its
// default beside the sessions.
func TestOmpProviders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("omp: {providers: {spark: {apiKey: op://Shared/spark/credential}}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Omp.Providers["spark"].APIKey; got != "op://Shared/spark/credential" {
		t.Errorf("apiKey = %q", got)
	}
	if !strings.HasSuffix(c.Omp.ModelsFile, filepath.Join(".omp", "agent", "models.yml")) {
		t.Errorf("modelsFile = %q", c.Omp.ModelsFile)
	}
}

func TestAgeIdentityRefs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := "secret: {vault: S, ageVaults: [V], store: {read: [r, -a], search: [s]}, ageIdentities: [{recipient: age1x, ref: op://V/i/f}, {pathRegex: /repo/, ref: \"file:///home/me/keys.txt\"}, {recipient: age1y, ref: \"store://\"}, {recipient: age1z, ref: store://keys/age}]}"
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
	if got := c.Secret.Store.Read; len(got) != 2 || len(c.Secret.Store.Search) != 1 || c.Secret.AgeIdentities[3].Ref != "store://keys/age" {
		t.Errorf("store = %+v, refs = %+v", c.Secret.Store, c.Secret.AgeIdentities)
	}
	if got := c.Secret.AgeVaults; len(got) != 1 || got[0] != "V" {
		t.Errorf("ageVaults = %q", got)
	}

	// an op:// entry outside secret.vault and secret.ageVaults is refused at
	// load, as is an age vault without a shared vault
	for name, raw := range map[string]string{
		"another vault":       "secret: {vault: S, ageIdentities: [{recipient: age1x, ref: op://V/i/f}]}",
		"no shared vault":     "secret: {ageVaults: [V]}",
		"a path, not a vault": "secret: {vault: S, ageVaults: [V/i]}",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Error("loads")
			}
		})
	}
}

func TestSecretFileRoots(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("secret: {fileRoots: [~/.codex/, /srv/creds]}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Secret.FileRoots; len(got) != 2 || got[0] != filepath.Join(home, ".codex") || got[1] != "/srv/creds" {
		t.Errorf("fileRoots = %q", got)
	}
	for name, raw := range map[string]string{
		"relative": "secret: {fileRoots: [creds]}",
		"the root": "secret: {fileRoots: [/]}",
		"empty":    "secret: {fileRoots: [\"\"]}",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "secret.fileRoots[0]") {
				t.Errorf("loads: %v", err)
			}
		})
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
	if got, ok := m.Model("claude-sonnet-5-5"); !ok || got.Input != 2 || got.Output != 10 || got.CacheRead != 0.2 || got.CacheWrite1h != 4 {
		t.Errorf("claude-sonnet-5-5 has its list price: %+v %v", got, ok)
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
		c.Board.Project != 12 || len(c.Board.Order) != 7 || !c.Board.Order[1].SubIssues || c.Board.Epics.Go != "in progress" || len(c.Board.Epics.Planning) != 1 || c.Board.StaleAfter.Hours() != 8760 {
		t.Errorf("config = %+v", c)
	}
	if d := c.Alerts.Discover; !d.Enabled() || d.Every.Duration != time.Hour || d.Label != "application.giantswarm.io/team" || !slices.Equal(d.Exclude, []string{"*-*", "*.*"}) {
		t.Errorf("alerts.discover %+v", d)
	}
	if tp := c.Teleport; !tp.Enabled() || tp.Proxy != "login.example.com:443" || tp.Auth != "github" || tp.Tsh != "tsh" ||
		tp.RenewBefore.Duration != 90*time.Minute || tp.WarnBefore.Duration != time.Hour || tp.Every.Duration != 10*time.Minute {
		t.Errorf("teleport %+v", tp)
	}
	if want := []alerts.Quiet{{Cluster: "t-*"}, OtherTeamsNotify}; !slices.Equal(c.Alerts.Quiet, want) ||
		!slices.Contains(c.Alerts.Ignore, "InhibitionOutsideWorkingHours") {
		t.Errorf("alerts quiet %+v, ignore %q", c.Alerts.Quiet, c.Alerts.Ignore)
	}
	if pd := c.Alerts.PagerDuty; !pd.Enabled() || pd.Server != "pd" || !slices.Equal(pd.Services, []string{"PABC123"}) || pd.Every.Duration != time.Minute {
		t.Errorf("alerts.pagerduty %+v", pd)
	}
	if u := c.Capacity.Usage; !u.Enabled() || len(u.FiveHour) != 3 || len(u.SevenDay) != 2 || u.HighPercent != 95 {
		t.Errorf("capacity.usage %+v", u)
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
	if c.MemcapCPUQuota(24) != "1200%" || c.MemcapCPUQuota(3) != "150%" || c.MemcapCPUQuota(0) != "" || c.Memcap.CPUWeight != DefaultMemcapCPUWeight {
		t.Errorf("memcap CPU: quota %q on 24, %q on 3, %q unknown, weight %d", c.MemcapCPUQuota(24), c.MemcapCPUQuota(3), c.MemcapCPUQuota(0), c.Memcap.CPUWeight)
	}
	const quota = "800%"
	c.Memcap.CPUQuota = quota
	if c.MemcapCPUQuota(24) != quota || c.MemcapCPUQuota(0) != quota {
		t.Errorf("memcap.cpuQuota set: %q, %q", c.MemcapCPUQuota(24), c.MemcapCPUQuota(0))
	}
	if _, err := Load(writeTemp(t, "kube: {contextTemplate: login.example.com}\n")); err == nil {
		t.Error("a context template without {installation} loads")
	}
	kc, err := Load(writeTemp(t, "kube: {kubeconfig: [~/.kube/config, /srv/kube/teleport.yaml]}\nalerts: {kubeconfig: [~/.local/state/beekeeper/kube/teleport.yaml]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if want := []string{filepath.Join(home, ".kube", "config"), "/srv/kube/teleport.yaml"}; !slices.Equal(kc.Kube.Kubeconfig, want) {
		t.Errorf("kube.kubeconfig = %q, want %q", kc.Kube.Kubeconfig, want)
	}
	if want := []string{filepath.Join(home, ".local", "state", "beekeeper", "kube", "teleport.yaml")}; !slices.Equal(kc.Alerts.Kubeconfig, want) {
		t.Errorf("alerts.kubeconfig = %q, want %q", kc.Alerts.Kubeconfig, want)
	}
}

func TestSupervisorOwnership(t *testing.T) {
	c, err := Load(writeTemp(t, "supervisor: {ownership: {review: [\"o/*-configs\"]}}"))
	if err != nil {
		t.Fatal(err)
	}
	// Without a team, no team files to read.
	if o := c.Supervisor.Ownership; o.TeamFiles != "" || o.OwnerWait.Duration != 24*time.Hour {
		t.Errorf("ownership without a team = %+v", o)
	}
	c, err = Load(writeTemp(t, "supervisor: {ownership: {team: team-a, ownerWait: 2h}}"))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Supervisor.Ownership
	if repo, dir := o.TeamFilesRepo(); o.TeamFiles != DefaultTeamFiles || repo != "giantswarm/github" || dir != "repositories" || o.OwnerWait.Duration != 2*time.Hour {
		t.Errorf("ownership = %+v (%s, %s)", o, repo, dir)
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

// A store:// reference without its secret.store section loads: only the
// commands that follow it fail, each said once by Incomplete.
func TestIncompleteStoreRef(t *testing.T) {
	for raw, want := range map[string]string{
		"secret: {ageIdentities: [{recipient: age1x, ref: store://keys/age}]}":                              "takes secret.store.read",
		"secret: {store: {read: [r]}, ageIdentities: [{recipient: age1x, ref: \"store://\"}]}":              "takes secret.store.search",
		"secret: {store: {read: [r], search: [s]}, ageIdentities: [{recipient: age1x, ref: \"store://\"}]}": "",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		got := c.Incomplete()
		switch {
		case want == "" && len(got) != 0:
			t.Errorf("%s: incomplete %v", raw, got)
		case want != "" && (len(got) != 1 || !strings.Contains(got[0], want)):
			t.Errorf("%s: incomplete %v, want one %q", raw, got, want)
		}
	}
}

func TestProfileFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	raw := "agents:\n  profile: minimal\n  profiles:\n    lean: {tools: [Read, Grep], plugins: [beekeeper, base], mcpServers: [pro]}\nsupervisor: {profile: lean}\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.Agents.ProfileFor(c.Agents.Profile); !ok || !slices.Contains(p.Tools, "SendMessage") || !slices.Equal(p.Plugins, []string{"beekeeper"}) ||
		slices.Contains(p.Tools, "AskUserQuestion") || slices.Contains(p.Tools, "Agent") {
		t.Errorf("built-in minimal = %+v %v", p, ok)
	}
	if p, ok := c.Agents.ProfileFor(c.Supervisor.Profile); !ok || !slices.Equal(p.Tools, []string{"Read", "Grep"}) || !slices.Equal(p.MCPServers, []string{"pro"}) {
		t.Errorf("desk profile lean = %+v %v", p, ok)
	}
	if _, ok := c.Agents.ProfileFor(c.Guide.Profile); ok {
		t.Error("a role without a profile resolved one")
	}
	c.Agents.Profiles[MinimalProfile] = Profile{Tools: []string{"Glob"}}
	if p, _ := c.Agents.ProfileFor(MinimalProfile); !slices.Equal(p.Tools, []string{"Glob"}) {
		t.Errorf("the desk's minimal did not replace the built-in one: %+v", p)
	}
}

// A purpose's manifest template is apps.template with the purpose for
// {name}, or apps.manifest's file as manifest.template.json; none without
// the declaring repository or a {name}.
func TestAppsTemplateOf(t *testing.T) {
	const manifest, repo = "apps/{name}/manifest.json", "o/r"
	for _, tc := range []struct {
		apps Apps
		want string
	}{
		{Apps{Repo: repo, Manifest: manifest}, "apps/portal/manifest.template.json"},
		{Apps{Repo: repo, Manifest: manifest, Template: "templates/{name}.json"}, "templates/portal.json"},
		{Apps{Manifest: manifest}, ""},
		{Apps{Repo: repo, Manifest: "apps/{name}/manifest.yaml"}, ""},
		{Apps{Repo: repo, Template: "templates/portal.json"}, ""},
	} {
		if got := tc.apps.TemplateOf("portal"); got != tc.want {
			t.Errorf("%+v.TemplateOf(portal) = %q, want %q", tc.apps, got, tc.want)
		}
	}
}

// A configured capacity.floor or capacity.ceiling of 0 is kept; unset takes
// DefaultFloor (clamped to a lower configured ceiling) and DefaultCeiling.
func TestCapacityZeroAndUnset(t *testing.T) {
	for _, tc := range []struct {
		name               string
		raw                string
		wantFloor, wantMax int
	}{
		{"omitted", "", DefaultFloor, DefaultCeiling},
		{"empty map", "capacity: {}\n", DefaultFloor, DefaultCeiling},
		{"null keys", "capacity: {floor: null, ceiling: null}\n", DefaultFloor, DefaultCeiling},
		{"both zero", "capacity: {floor: 0, ceiling: 0}\n", 0, 0},
		{"ceiling zero alone", "capacity: {ceiling: 0}\n", 0, 0},
		{"ceiling under default floor", "capacity: {ceiling: 3}\n", 3, 3},
		{"floor zero alone", "capacity: {floor: 0}\n", 0, DefaultCeiling},
		{"floor zero, ceiling set", "capacity: {floor: 0, ceiling: 4}\n", 0, 4},
	} {
		c, err := Load(writeTemp(t, tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if c.Capacity.Floor != tc.wantFloor || c.Capacity.Ceiling != tc.wantMax {
			t.Errorf("%s: got floor %d, ceiling %d; want %d, %d", tc.name, c.Capacity.Floor, c.Capacity.Ceiling, tc.wantFloor, tc.wantMax)
		}
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	c, err := Set(p, []Setting{{"capacity.floor", "0"}, {"capacity.ceiling", "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Capacity.Floor != 0 || c.Capacity.Ceiling != 0 {
		t.Errorf("Set returned floor %d, ceiling %d; want 0, 0", c.Capacity.Floor, c.Capacity.Ceiling)
	}
	if loaded, err := Load(p); err != nil || loaded.Capacity.Floor != 0 || loaded.Capacity.Ceiling != 0 {
		t.Errorf("Load after Set = %+v, %v; want floor 0, ceiling 0", loaded, err)
	}
}

func TestCapacityUsageAndDerive(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	raw := `capacity:
  usage:
    command: ["~/bin/read-usage", "~/etc/usage.json", "--json"]
    fiveHour:
      - {upTo: 50, floor: 6, ceiling: 12}
      - {upTo: 80, ceiling: 4}
      - {upTo: 95, floor: 0, ceiling: 2}
    sevenDay:
      - {upTo: 70, floor: 5, ceiling: 10}
      - {upTo: 90, floor: 2, ceiling: 3}
`
	c, err := Load(writeTemp(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	u := c.Capacity.Usage
	if !u.Enabled() || u.Command[0] != filepath.Join(home, "bin", "read-usage") ||
		u.Command[1] != filepath.Join(home, "etc", "usage.json") || u.Command[2] != "--json" {
		t.Fatalf("command = %q", u.Command)
	}
	if u.Every.Duration != DefaultUsageEvery || u.Timeout.Duration != DefaultUsageTimeout ||
		u.ReportEvery.Duration != DefaultUsageReportEvery || u.HighPercent != DefaultUsageHighPercent || u.HighPercentSet() {
		t.Errorf("defaults = every %s timeout %s reportEvery %s highPercent %g (set %v)",
			u.Every, u.Timeout, u.ReportEvery, u.HighPercent, u.HighPercentSet())
	}
	// Band without floor clamps default floor (5) to its ceiling (4); explicit floor: 0 stays 0.
	if u.FiveHour[1].Floor != 4 || u.FiveHour[2].Floor != 0 {
		t.Errorf("band floors = %+v", u.FiveHour)
	}

	num := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name               string
		five, seven        *float64
		wantFloor, wantMax int
		wantHigh           bool
	}{
		{"first 5h band, 7d unreported", num(20), nil, 6, 12, false},
		{"first 5h + first 7d takes min", num(20), num(40), 5, 10, false},
		{"boundary 50 enters second 5h band", num(50), num(40), 4, 4, false},
		{"second 7d band caps first 5h band", num(20), num(75), 2, 3, false},
		{"third 5h band keeps floor 0", num(85), num(40), 0, 2, false},
		{"past last 7d band yields 0/0 without high", num(20), num(90), 0, 0, false},
		{"at highPercent triggers USAGE HIGH", num(95), num(40), 0, 0, true},
		{"above 100 triggers USAGE HIGH", num(102.5), num(40), 0, 0, true},
	} {
		got, high := c.Capacity.Derive(tc.five, tc.seven)
		if got.Floor != tc.wantFloor || got.Ceiling != tc.wantMax || high != tc.wantHigh {
			t.Errorf("%s: got (%d, %d, high=%v), want (%d, %d, high=%v)",
				tc.name, got.Floor, got.Ceiling, high, tc.wantFloor, tc.wantMax, tc.wantHigh)
		}
	}

	// An explicit top-level ceiling (such as capacity.ceiling: 0) still caps the bands.
	paused, err := Load(writeTemp(t, "capacity:\n  ceiling: 0\n  usage:\n    command: [reader]\n    fiveHour: [{upTo: 80, floor: 3, ceiling: 6}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, high := paused.Capacity.Derive(num(10), nil); got.Floor != 0 || got.Ceiling != 0 || high {
		t.Errorf("explicit ceiling 0 with band: got (%d, %d, high=%v), want (0, 0, false)", got.Floor, got.Ceiling, high)
	}

	// Negative reportEvery turns periodic USAGE watch lines off without failing validation.
	quiet, err := Load(writeTemp(t, "capacity: {usage: {command: [reader], reportEvery: -1m, highPercent: 98}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if quiet.Capacity.Usage.ReportEvery.Duration >= 0 || !quiet.Capacity.Usage.HighPercentSet() || quiet.Capacity.Usage.HighPercent != 98 {
		t.Errorf("quiet usage = %+v", quiet.Capacity.Usage)
	}
	if got, high := quiet.Capacity.Derive(num(96), nil); got.Floor != DefaultFloor || got.Ceiling != DefaultCeiling || high {
		t.Errorf("no bands below custom highPercent 98: got (%d, %d, high=%v)", got.Floor, got.Ceiling, high)
	}
	if got, high := quiet.Capacity.Derive(num(98), nil); got.Floor != 0 || got.Ceiling != 0 || !high {
		t.Errorf("at custom highPercent 98: got (%d, %d, high=%v)", got.Floor, got.Ceiling, high)
	}
}

func TestCapacityProvidersAndChoices(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	raw := `capacity:
  floor: 5
  ceiling: 10
  providers:
    claude:
      floor: 5
      ceiling: 10
      command: ["~/bin/claude-usage"]
      staleAfter: 15m
      fiveHourMax: 90
      sevenDayMax: 99
      paceMargin: 2
      fiveHour:
        - {upTo: 70, floor: 5, ceiling: 10}
        - {upTo: 85, floor: 2, ceiling: 4}
        - {upTo: 95, floor: 0, ceiling: 1}
      sevenDay:
        - {upTo: 85, floor: 5, ceiling: 10}
        - {upTo: 95, floor: 1, ceiling: 3}
      paceCaps:
        - {overBy: 5, floor: 1, ceiling: 3}
        - {overBy: 10, floor: 0, ceiling: 1}
    agy:
      ceiling: 2
      fiveHourMax: 90
      sevenDayMax: 99
  choices:
    - {harness: claude, model: claude-opus-4-7}
    - {harness: agy, model: claude-opus-4-6-thinking}
    - {harness: agy, model: gemini-3-1-pro-high, skipPace: true, sevenDayMax: 80}
`
	c, err := Load(writeTemp(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	const (
		provClaude = "claude"
		provAgy    = "agy"
	)
	if got := c.Capacity.ProviderNames(); !slices.Equal(got, []string{provClaude, provAgy}) {
		t.Fatalf("ProviderNames = %v, want [claude agy]", got)
	}
	cl := c.Capacity.Providers[provClaude]
	if cl.Command[0] != filepath.Join(home, "bin", "claude-usage") || cl.EffectiveStaleAfter() != 15*time.Minute {
		t.Errorf("claude provider = %+v", cl)
	}
	agy := c.Capacity.Providers[provAgy]
	if agy.Floor != 0 || agy.Ceiling != 2 || !slices.Equal(agy.Command, DefaultAgyUsageCommand) {
		t.Errorf("agy provider = %+v", agy)
	}
	if got := c.Capacity.Choices[0]; got.EffectiveProvider() != provClaude || got.EffectivePaceMargin(cl) != 2 {
		t.Errorf("choice[0] = %+v", got)
	}
	if got := c.Capacity.Choices[1]; got.EffectiveProvider() != provAgy || got.EffectiveBucket(agy) != DefaultAgyClaudeBucket {
		t.Errorf("choice[1] = %+v (bucket %q)", got, got.EffectiveBucket(agy))
	}
	if got := c.Capacity.Choices[2]; got.EffectiveProvider() != provAgy || got.EffectiveBucket(agy) != DefaultAgyGeminiBucket || !got.SkipPace {
		t.Errorf("choice[2] = %+v (bucket %q)", got, got.EffectiveBucket(agy))
	}
	if max7d, ok := c.Capacity.Choices[2].EffectiveWorkerSevenDayMax(); !ok || max7d != 80 {
		t.Errorf("choice[2] worker sevenDayMax = %g, %v", max7d, ok)
	}

	num := func(v float64) *float64 { return &v }
	// PaceCaps lower claude's floor/ceiling when 7d is ahead of pace by >= 5% or >= 10%.
	if dp, high := cl.DeriveWithPace(num(20), num(56), num(50)); dp.Floor != 1 || dp.Ceiling != 3 || high {
		t.Errorf("paceCap +6%%: got (%d, %d, high=%v), want (1, 3, false)", dp.Floor, dp.Ceiling, high)
	}
	if dp, high := cl.DeriveWithPace(num(20), num(61), num(50)); dp.Floor != 0 || dp.Ceiling != 1 || high {
		t.Errorf("paceCap +11%%: got (%d, %d, high=%v), want (0, 1, false)", dp.Floor, dp.Ceiling, high)
	}
	// fiveHourMax (90) yields (0, 0) without high (< 95); at 95 high is true.
	if dp, high := cl.DeriveWithPace(num(91), num(40), num(50)); dp.Floor != 0 || dp.Ceiling != 0 || high {
		t.Errorf("at fiveHourMax 91%%: got (%d, %d, high=%v), want (0, 0, false)", dp.Floor, dp.Ceiling, high)
	}
	if dp, high := cl.DeriveWithPace(num(95), num(40), num(50)); dp.Floor != 0 || dp.Ceiling != 0 || !high {
		t.Errorf("at highPercent 95%%: got (%d, %d, high=%v), want (0, 0, true)", dp.Floor, dp.Ceiling, high)
	}

	// A self-hosted provider fails closed: no subscription fields, no
	// half-configured serving mode, no subscription choice on it, and its
	// ceiling follows a ~/ serveModeCommand and a floor of its own.
	t.Run("self-hosted rejections", func(t *testing.T) {
		for name, raw := range map[string]string{
			"with a usage command":    "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      command: [r]\n",
			"with a usage threshold":  "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      fiveHourMax: 90\n",
			"with buckets":            "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      buckets: {m: b}\n",
			"negative serve stale":    "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCommand: [r]\n      serveModeCeilings: {full: 2}\n      serveModeStaleAfter: -1m\n",
			"empty mode name":         "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCommand: [r]\n      serveModeCeilings: {\" \": 2}\n",
			"negative mode ceiling":   "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCommand: [r]\n      serveModeCeilings: {full: -1}\n",
			"command without modes":   "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCommand: [r]\n",
			"modes without command":   "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCeilings: {full: 2}\n",
			"modes without switch":    "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCommand: [r]\n      serveModeModes: {full: {sessions: 2}}\n",
			"switch without reader":   "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeSwitchCommand: [s]\n      serveModeModes: {full: {sessions: 2}}\n",
			"negative switch timeout": "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCommand: [r]\n      serveModeCeilings: {full: 2}\n      serveModeSwitchTimeout: -1m\n",
			"serve mode on claude":    "capacity:\n  providers:\n    claude:\n      ceiling: 2\n      serveModeCommand: [r]\n      serveModeCeilings: {full: 2}\n",
			"modes on claude":         "capacity:\n  providers:\n    claude:\n      ceiling: 2\n      serveModeModes: {full: {sessions: 2}}\n",
			"switch timeout on agy":   "capacity:\n  providers:\n    agy:\n      ceiling: 2\n      serveModeSwitchTimeout: 5m\n",
			"claude choice on local":  "capacity:\n  providers:\n    local-model: {kind: self-hosted, ceiling: 2}\n  choices:\n    - {harness: claude, model: m, provider: local-model}\n",
			"claude as self-hosted":   "capacity:\n  providers:\n    claude: {kind: self-hosted, ceiling: 2}\n",
			"agy as self-hosted":      "capacity:\n  providers:\n    agy: {kind: self-hosted, ceiling: 2}\n",
			"codex as self-hosted":    "capacity:\n  providers:\n    codex: {kind: self-hosted, ceiling: 2}\n",
			"agy choice on local":     "capacity:\n  providers:\n    local-model: {kind: self-hosted, ceiling: 2}\n  choices:\n    - {harness: agy, model: m, provider: local-model}\n",
			"codex choice on local":   "capacity:\n  providers:\n    local-model: {kind: self-hosted, ceiling: 2}\n  choices:\n    - {harness: codex, model: m, provider: local-model}\n",
		} {
			if _, err := Load(writeTemp(t, raw)); err == nil {
				t.Errorf("%s: loaded invalid config", name)
			}
		}
	})
	t.Run("self-hosted defaults", func(t *testing.T) {
		raw := "capacity:\n  ceiling: 10\n  providers:\n    local-model:\n      kind: self-hosted\n      serveModeCommand: [\"~/bin/read-serve-mode\"]\n      serveModeCeilings: {full: 3}\n"
		c, err := Load(writeTemp(t, raw))
		if err != nil {
			t.Fatal(err)
		}
		p := c.Capacity.Providers["local-model"]
		if p.Floor != 0 {
			t.Errorf("self-hosted floor = %d, want 0", p.Floor)
		}
		if p.Ceiling != 10 {
			t.Errorf("self-hosted ceiling = %d, want the configured 10", p.Ceiling)
		}
		if p.ServeModeCommand[0] != filepath.Join(home, "bin", "read-serve-mode") {
			t.Errorf("serveModeCommand = %v, want the home-expanded path", p.ServeModeCommand)
		}
		// A mode whose ceiling is under the provider's configured floor
		// clamps the floor to it, so floor never exceeds ceiling.
		clRaw := "capacity:\n  providers:\n    local-model:\n      kind: self-hosted\n      floor: 2\n      ceiling: 5\n      serveModeCommand: [r]\n      serveModeCeilings: {full: 5, tiny: 1}\n"
		cc, err := Load(writeTemp(t, clRaw))
		if err != nil {
			t.Fatal(err)
		}
		cp := cc.Capacity.Providers["local-model"]
		if got := cp.WithServeMode(ServeModeReading{Mode: "tiny"}); got.Ceiling != 1 || got.Floor != 1 {
			t.Errorf("WithServeMode(tiny) = (%d, %d), want (1, 1)", got.Ceiling, got.Floor)
		}
		// An unknown mode keeps the configured pair.
		if got := cp.WithServeMode(ServeModeReading{Mode: "other"}); got.Ceiling != 5 || got.Floor != 2 {
			t.Errorf("WithServeMode(unknown) = (%d, %d), want (5, 2)", got.Ceiling, got.Floor)
		}
	})

	for name, bad := range map[string]string{
		"unknown provider in choice": "capacity:\n  providers:\n    claude: {command: [r]}\n  choices:\n    - {harness: agy, model: m}\n",
		"choices without providers":  "capacity:\n  choices:\n    - {harness: claude, model: m}\n",
		"missing model in choice":    "capacity:\n  providers:\n    claude: {command: [r]}\n  choices:\n    - {harness: claude}\n",
		"provider without command":   "capacity:\n  providers:\n    claude: {fiveHourMax: 90}\n",
		"descending paceCaps":        "capacity:\n  providers:\n    claude:\n      command: [r]\n      paceCaps:\n        - {overBy: 10, ceiling: 2}\n        - {overBy: 5, ceiling: 1}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, bad)); err == nil {
				t.Errorf("%s: loaded invalid config", name)
			}
		})
	}
}

func TestReviewHeadroomConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		bad   bool
	}{
		{"0", 0, false}, {"3", 3, false}, {"-1", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("capacity:\n  reviewHeadroom: "+tc.value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "capacity.reviewHeadroom") {
					t.Fatalf("validation = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Capacity.ReviewHeadroom != tc.want {
				t.Fatalf("headroom %d, want %d", c.Capacity.ReviewHeadroom, tc.want)
			}
		})
	}
}

func TestSupervisorFillRetryEvery(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want time.Duration
	}{
		{"supervisor: {}", 5 * time.Minute},
		{"supervisor: {fillRetryEvery: 2m}", 2 * time.Minute},
	} {
		c, err := Load(writeTemp(t, test.raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Supervisor.FillRetryEvery.Duration; got != test.want {
			t.Fatalf("fill retry %s, want %s", got, test.want)
		}
	}
}
