package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

// Scenario is one scenario file (contract §4).
type Scenario struct {
	Name             string         `yaml:"name"`
	Concept          string         `yaml:"concept"`
	Tags             []string       `yaml:"tags"`
	Description      string         `yaml:"description"`
	PreConfiguration map[string]any `yaml:"pre_configuration"`
	ExpectViolations []string       `yaml:"expect_violations"`
	PostChecks       PostChecks     `yaml:"post_checks"`
	Steps            []Step         `yaml:"steps"`

	// Path is the file the scenario was loaded from.
	Path string `yaml:"-"`
	// Pre is the pre_configuration merged onto its fixture and decoded.
	Pre *Pre `yaml:"-"`
}

// PostChecks switch off the checks made after every scenario (contract §3.6). Unset means on;
// no_github_writes can't be switched off.
type PostChecks struct {
	NoGitHubWrites    *bool `yaml:"no_github_writes"`
	NoReconcileErrors *bool `yaml:"no_reconcile_errors"`
	InvariantsHold    *bool `yaml:"invariants_hold"`
	Settled           *bool `yaml:"settled"`
}

func on(b *bool) bool { return b == nil || *b }

// Step calls a tool with arguments and states what is expected of its result.
type Step struct {
	ID          string         `yaml:"id"`
	Description string         `yaml:"description"`
	Tool        string         `yaml:"tool"`
	Args        map[string]any `yaml:"args"`
	Expected    *Expected      `yaml:"expected"`
	// Timeout bounds the step (all its polls); default stepTimeout.
	Timeout string `yaml:"timeout"`

	timeout time.Duration
}

// stepTimeout bounds a step without its own timeout.
const stepTimeout = 30 * time.Second

// Pre is a scenario's decoded pre_configuration, merged onto its fixture.
type Pre struct {
	Fixture string
	Clock   time.Time
	Config  Config
	// GitHub is the fake GitHub's seed, decoded strictly by the fake.
	GitHub map[string]any
	// JEV is the fake JEV's seed, decoded strictly by the fake.
	JEV     map[string]any
	Engine  EngineConfig
	History HistorySeed
}

// Config holds the configuration objects as Flux would apply them, kept as maps so that config_apply
// can merge onto them.
type Config struct {
	Installation map[string]any
	Teams        []map[string]any
	People       []map[string]any
}

// TeamConfig is a Team object's configuration: its name and spec.
type TeamConfig struct {
	Name string `json:"name"`
	v1alpha1.TeamSpec
}

// EngineConfig configures the engine replicas.
type EngineConfig struct {
	// Replicas is 1 or 2; leader election runs only with 2.
	Replicas int `json:"replicas"`
}

// HistorySeed are rows written to Postgres before the engine starts.
type HistorySeed struct {
	CI     []CIDurations    `json:"ci"`
	Stages []StageDurations `json:"stages"`
}

// CIDurations are a repo's pipeline durations.
type CIDurations struct {
	Repo      string   `json:"repo"`
	Durations []string `json:"durations"`
}

// StageDurations are a team's durations of a phase.
type StageDurations struct {
	Team      string   `json:"team"`
	Phase     string   `json:"phase"`
	Durations []string `json:"durations"`
}

// InstallationSpec decodes the installation configuration strictly.
func (c Config) InstallationSpec() (v1alpha1.InstallationSpec, error) {
	var s v1alpha1.InstallationSpec
	err := strictJSON(c.Installation, &s)
	return s, err
}

// TeamConfigs decodes the teams strictly.
func (c Config) TeamConfigs() ([]TeamConfig, error) {
	out := make([]TeamConfig, len(c.Teams))
	for i, t := range c.Teams {
		if err := strictJSON(t, &out[i]); err != nil {
			return nil, fmt.Errorf("teams[%d]: %w", i, err)
		}
		if out[i].Name == "" {
			return nil, fmt.Errorf("teams[%d]: missing name", i)
		}
	}
	return out, nil
}

// PersonSpecs decodes the people strictly.
func (c Config) PersonSpecs() ([]v1alpha1.PersonSpec, error) {
	out := make([]v1alpha1.PersonSpec, len(c.People))
	for i, p := range c.People {
		if err := strictJSON(p, &out[i]); err != nil {
			return nil, fmt.Errorf("people[%d]: %w", i, err)
		}
		if out[i].Login == "" {
			return nil, fmt.Errorf("people[%d]: missing login", i)
		}
	}
	return out, nil
}

// Durations parses durations such as 20m.
func Durations(ds []string) ([]time.Duration, error) {
	out := make([]time.Duration, len(ds))
	for i, s := range ds {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, err
		}
		out[i] = d
	}
	return out, nil
}

// strictJSON converts v to out through JSON, rejecting unknown fields.
func strictJSON(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

// LoadError lists every problem found while loading scenarios.
type LoadError struct{ Problems []string }

func (e *LoadError) Error() string {
	return fmt.Sprintf("%d problems:\n  %s", len(e.Problems), strings.Join(e.Problems, "\n  "))
}

// LoadDir strictly loads every scenario under root (root/<concept>/<name>.yaml), with the fixtures in
// root/fixtures. Every problem in every file is reported; the scenarios are returned sorted by name.
func LoadDir(root string) ([]*Scenario, error) {
	var (
		out      []*Scenario
		problems []string
		names    = map[string]string{}
	)
	fixtures := map[string]map[string]any{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "fixtures" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			return nil
		}
		s, errs := loadFile(path, root, fixtures)
		for _, e := range errs {
			problems = append(problems, fmt.Sprintf("%s: %s", path, e))
		}
		if s == nil {
			return nil
		}
		if prev, dup := names[s.Name]; dup {
			problems = append(problems, fmt.Sprintf("%s: duplicate name, also in %s", path, prev))
		}
		names[s.Name] = path
		if len(errs) == 0 {
			out = append(out, s)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(problems)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(problems) > 0 {
		return out, &LoadError{Problems: problems}
	}
	return out, nil
}

// LoadFile strictly loads one scenario; root is the scenarios directory holding fixtures/.
func LoadFile(path, root string) (*Scenario, error) {
	s, errs := loadFile(path, root, map[string]map[string]any{})
	if len(errs) > 0 {
		p := make([]string, len(errs))
		for i, e := range errs {
			p[i] = fmt.Sprintf("%s: %s", path, e)
		}
		return nil, &LoadError{Problems: p}
	}
	return s, nil
}

// Parse strictly decodes and checks a scenario from YAML; path names it in problems and must end in
// <concept>/<name>.yaml.
func Parse(data []byte, path, root string) (*Scenario, error) {
	s, errs := parse(data, path, root, map[string]map[string]any{})
	if len(errs) > 0 {
		p := make([]string, len(errs))
		for i, e := range errs {
			p[i] = e.Error()
		}
		return nil, &LoadError{Problems: p}
	}
	return s, nil
}

func loadFile(path, root string, fixtures map[string]map[string]any) (*Scenario, []error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: a scenario file under the suite's scenario directory
	if err != nil {
		return nil, []error{err}
	}
	return parse(b, path, root, fixtures)
}

func parse(b []byte, path, root string, fixtures map[string]map[string]any) (*Scenario, []error) {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, []error{fmt.Errorf("decode: %w", err)}
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		fail("more than one YAML document")
	}
	s.Path = path

	base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".yaml"), ".yml")
	if s.Name == "" {
		fail("missing name")
	} else if s.Name != base {
		fail("name %q != file name %q", s.Name, base)
	}
	if !in(s.Concept, Concepts) {
		fail("unknown concept %q", s.Concept)
	}
	if dir := filepath.Base(filepath.Dir(path)); dir != s.Concept {
		fail("concept %q but directory %q", s.Concept, dir)
	}
	if !strings.HasPrefix(s.Name, s.Concept+"-") {
		fail("name not prefixed by concept")
	}
	if strings.TrimSpace(s.Description) == "" {
		fail("missing description")
	} else if !strings.Contains(s.Description, "Given") || !strings.Contains(s.Description, "When") || !strings.Contains(s.Description, "Then") {
		fail("description lacks Given/When/Then")
	}
	if !in("phase1", s.Tags) {
		fail("missing tag phase1")
	}
	for _, v := range s.ExpectViolations {
		if !in(v, Invariants) {
			fail("unknown invariant %q in expect_violations", v)
		}
	}
	if s.PostChecks.NoGitHubWrites != nil && !*s.PostChecks.NoGitHubWrites {
		fail("post_checks.no_github_writes can't be switched off")
	}

	pre, err := decodePre(s.PreConfiguration, root, fixtures)
	if err != nil {
		fail("pre_configuration: %v", err)
	}
	s.Pre = pre

	errs = append(errs, checkSteps(&s)...)
	return &s, errs
}

// checkSteps checks every step against the registered tools and the vocabulary.
func checkSteps(s *Scenario) []error {
	var errs []error
	if len(s.Steps) == 0 {
		errs = append(errs, errors.New("no steps"))
	}
	seen := map[string]bool{}
	asserts := 0
	for i := range s.Steps {
		st := &s.Steps[i]
		where := fmt.Sprintf("step %d (%s)", i, st.ID)
		fail := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("%s: %s", where, fmt.Sprintf(format, a...)))
		}
		if st.ID == "" {
			fail("missing id")
		}
		if seen[st.ID] {
			fail("duplicate id")
		}
		if st.Timeout != "" {
			d, err := time.ParseDuration(st.Timeout)
			if err != nil || d <= 0 {
				fail("bad timeout %q", st.Timeout)
			}
			st.timeout = d
		}
		tool, ok := Lookup(st.Tool)
		if !ok {
			fail("unknown tool %q", st.Tool)
			seen[st.ID] = true
			continue
		}
		for _, k := range sortedKeys(st.Args) {
			v := st.Args[k]
			if !in(k, tool.Args) {
				fail("tool %s has no arg %q", st.Tool, k)
				continue
			}
			if e, ok := enums[st.Tool][k]; ok {
				if sv, _ := v.(string); !in(sv, e) {
					fail("%s.%s=%v not in %v", st.Tool, k, v, e)
				}
			}
			switch {
			case k == argSinceStep:
				if sv, _ := v.(string); !seen[sv] {
					fail("since_step %q is not an earlier step", v)
				}
			case k == argType && st.Tool == "events":
				if sv, _ := v.(string); !in(sv, EventTypes) {
					fail("unknown event type %q", v)
				}
			case k == argName && st.Tool == "metric":
				if sv, _ := v.(string); !in(sv, Metrics) {
					fail("unknown metric %q", v)
				}
			case k == "labels" && st.Tool == "metric":
				l, ok := v.(map[string]any)
				if !ok {
					fail("metric labels must be a map")
					break
				}
				if st.Args[argName] == "engine_invariant_violations" {
					if iv, _ := l["invariant"].(string); iv != "" && !in(iv, Invariants) {
						fail("unknown invariant label %q", iv)
					}
				}
			case k == "by" && st.Tool == toolClockAdvance:
				if d, err := time.ParseDuration(fmt.Sprint(v)); err != nil || d < 0 {
					fail("bad duration %v", v)
				}
			case k == "deliver":
				switch dv := v.(type) {
				case string:
					if !in(dv, deliverModes) {
						fail("bad deliver %q", dv)
					}
				case map[string]any:
					d, ok := dv["delay"]
					if !ok || len(dv) != 1 {
						fail("deliver map must be {delay: d}")
					} else if _, err := time.ParseDuration(fmt.Sprint(d)); err != nil {
						fail("bad deliver delay %v", d)
					}
				default:
					fail("bad deliver %v", v)
				}
			}
		}
		if st.Tool == toolClockAdvance {
			if _, ok := st.Args["by"]; !ok {
				fail("clock_advance needs by")
			}
		}
		if st.Expected != nil {
			for _, p := range st.Expected.check() {
				fail("expected: %s", p)
			}
			if st.Expected.asserts() {
				asserts++
			}
		}
		seen[st.ID] = true
	}
	if asserts == 0 && len(s.Steps) > 0 {
		errs = append(errs, errors.New("no step asserts anything"))
	}
	return errs
}

// decodePre merges pre_configuration onto its fixture and decodes it strictly.
func decodePre(raw map[string]any, root string, fixtures map[string]map[string]any) (*Pre, error) {
	for k := range raw {
		if !in(k, preKeys) {
			return nil, fmt.Errorf("unknown key %q", k)
		}
	}
	name, _ := raw["fixture"].(string)
	if name != "base" {
		return nil, fmt.Errorf("fixture must be base, got %q", raw["fixture"])
	}
	fx, ok := fixtures[name]
	if !ok {
		var err error
		fx, err = loadFixture(filepath.Join(root, "fixtures", name+".yaml"))
		if err != nil {
			return nil, err
		}
		fixtures[name] = fx
	}
	over := clone(raw).(map[string]any)
	delete(over, "fixture")
	merged := Merge(fx, over).(map[string]any)
	return decodeMerged(name, merged)
}

func loadFixture(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: a fixture file under the suite's scenario directory
	if err != nil {
		return nil, fmt.Errorf("fixture: %w", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("fixture %s: %w", path, err)
	}
	for k := range m {
		if k == "fixture" || !in(k, preKeys) {
			return nil, fmt.Errorf("fixture %s: unknown key %q", path, k)
		}
	}
	return m, nil
}

func decodeMerged(fixture string, m map[string]any) (*Pre, error) {
	p := &Pre{Fixture: fixture, Engine: EngineConfig{Replicas: 1}}
	cs, _ := m["clock"].(string)
	t, err := time.Parse(time.RFC3339, cs)
	if err != nil {
		return nil, fmt.Errorf("clock: %v", err)
	}
	p.Clock = t.UTC()

	cfg, _ := m["config"].(map[string]any)
	for k := range cfg {
		if !in(k, []string{"installation", argTeams, argPeople}) {
			return nil, fmt.Errorf("config: unknown key %q", k)
		}
	}
	if inst, ok := cfg["installation"].(map[string]any); ok {
		p.Config.Installation = inst
	} else {
		return nil, errors.New("config.installation missing")
	}
	for _, k := range []string{argTeams, argPeople} {
		l, _ := cfg[k].([]any)
		for i, e := range l {
			em, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("config.%s[%d] is not a map", k, i)
			}
			if k == argTeams {
				p.Config.Teams = append(p.Config.Teams, em)
			} else {
				p.Config.People = append(p.Config.People, em)
			}
		}
	}
	spec, err := p.Config.InstallationSpec()
	if err != nil {
		return nil, fmt.Errorf("config.installation: %w", err)
	}
	if spec.Org == "" {
		return nil, errors.New("config.installation.org missing")
	}
	if _, err := p.Config.TeamConfigs(); err != nil {
		return nil, fmt.Errorf("config.%w", err)
	}
	if _, err := p.Config.PersonSpecs(); err != nil {
		return nil, fmt.Errorf("config.%w", err)
	}

	p.GitHub, _ = m["github"].(map[string]any)
	if p.GitHub == nil {
		return nil, errors.New("github missing")
	}
	p.JEV, _ = m["jev"].(map[string]any)

	if e, ok := m["engine"]; ok {
		if err := strictJSON(e, &p.Engine); err != nil {
			return nil, fmt.Errorf("engine: %w", err)
		}
		if p.Engine.Replicas == 0 {
			p.Engine.Replicas = 1
		}
	}
	if p.Engine.Replicas != 1 && p.Engine.Replicas != 2 {
		return nil, fmt.Errorf("engine.replicas must be 1 or 2, got %d", p.Engine.Replicas)
	}
	if h, ok := m["history"]; ok {
		if err := strictJSON(h, &p.History); err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		for _, c := range p.History.CI {
			if _, err := Durations(c.Durations); err != nil || c.Repo == "" {
				return nil, fmt.Errorf("history.ci %q: bad repo or durations %v", c.Repo, c.Durations)
			}
		}
		for _, st := range p.History.Stages {
			if _, err := Durations(st.Durations); err != nil || st.Team == "" || st.Phase == "" {
				return nil, fmt.Errorf("history.stages %s/%s: bad team, phase or durations", st.Team, st.Phase)
			}
		}
	}
	return p, nil
}

// HasTag reports whether the scenario has tag t.
func (s *Scenario) HasTag(t string) bool { return in(t, s.Tags) }

// ClockAdvance is the total of the scenario's clock_advance steps.
func (s *Scenario) ClockAdvance() time.Duration {
	var total time.Duration
	for _, st := range s.Steps {
		if st.Tool == toolClockAdvance {
			d, _ := time.ParseDuration(fmt.Sprint(st.Args["by"]))
			total += d
		}
	}
	return total
}

// Budget is the scenario's time budget once the pool is warm (contract §3.4): 1s, or 3s for
// scenarios that advance the clock by an hour or more.
func (s *Scenario) Budget() time.Duration {
	if s.ClockAdvance() >= time.Hour {
		return 3 * time.Second
	}
	return time.Second
}

// Filter keeps the scenarios of concept and tag (empty matches all).
func Filter(all []*Scenario, concept, tag string) []*Scenario {
	var out []*Scenario
	for _, s := range all {
		if (concept == "" || s.Concept == concept) && (tag == "" || s.HasTag(tag)) {
			out = append(out, s)
		}
	}
	return out
}
