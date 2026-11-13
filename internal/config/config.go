// Package config loads beekeeper's machine configuration: where its state
// lives, which shared resources sessions lease, the GitHub budget floor, the
// thresholds of the machine watch and which installations' alerts it reads.
//
// The file is $XDG_CONFIG_HOME/beekeeper/config.yaml (or --config, or
// $BEEKEEPER_CONFIG). Every field is optional; a missing file is the
// defaults unless explicitly named by $BEEKEEPER_CONFIG. The defaults are
// neutral: memory thresholds are fractions of the machine's RAM, swap or
// filesystem, and every organisation or desk choice
// (the production installation, the installations, the lanes, the role
// names) is unset until configured. docs/examples/config.yaml is a complete
// desk's configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Projects separates the person's clones from session checkouts.
type Projects struct {
	Dir       string   `yaml:"dir"`
	Worktrees []string `yaml:"worktrees"`
}

// Home names places for session state, logs and backups.
type Home struct {
	Places []string `yaml:"places"`
}

// Delivery configures observation of issue delivery. A zero limit is off.
type Delivery struct {
	WIPLimit     int      `yaml:"wipLimit"`
	EpicLimit    int      `yaml:"epicLimit"`
	EpicParallel int      `yaml:"epicParallel"`
	StallAfter   Duration `yaml:"stallAfter"`
}

// Config is the parsed configuration with the defaults applied.
type Config struct {
	Delivery Delivery `yaml:"delivery"`
	Projects Projects `yaml:"projects"`
	Home     Home     `yaml:"home"`
	// StateDir holds state.json, events.jsonl and the last snapshot.
	StateDir string `yaml:"stateDir"`
	// Fatal adds machine-specific outage probes to the independent sentinel.
	Fatal Fatal `yaml:"fatal"`
	// LeaseDir holds one directory per held lease (mkdir is the lock).
	LeaseDir string `yaml:"leaseDir"`
	// LeaseRound is the default round when a lease purpose names no duration (1h).
	LeaseRound Duration `yaml:"leaseRound"`
	// Identity is whose agents run here: every party this machine's
	// commands record carries it.
	Identity Identity `yaml:"identity"`
	// Serve configures the central instance (beekeeper serve).
	Serve Serve `yaml:"serve"`
	// Resources are the environments sessions lease besides the browser:
	// kind labs and shared installations.
	Resources []string `yaml:"resources"`
	// Labs maps a lab lease (one of Resources) to the kind cluster it
	// stands for: `lease list`, a claim and `free` name the cluster, and a
	// running kind cluster no lab lease maps is reported as unmapped.
	Labs map[string]string `yaml:"labs"`
	// GrantTTL is how long a grant stays claimable once its resource is free.
	GrantTTL Duration `yaml:"grantTTL"`
	// Shell is the shell the hook runs a rewritten command in (default:
	// $SHELL, else sh).
	Shell string `yaml:"shell"`
	// MaxKindClusters is how many kind clusters the machine runs at most
	// (default: one per 40 GiB of RAM, at least one).
	MaxKindClusters int `yaml:"maxKindClusters"`

	Kube Kube `yaml:"kube"`
	// Teleport is the login the kube contexts reach the installations
	// through, and its keeper.
	Teleport Teleport `yaml:"teleport"`

	GitHub   GitHub    `yaml:"github"`
	Watch    Watch     `yaml:"watch"`
	Disk     Disk      `yaml:"disk"`
	Overlaps Overlaps  `yaml:"overlaps"`
	Claude   Claude    `yaml:"claude"`
	Omp      Omp       `yaml:"omp"`
	Agy      Agy       `yaml:"agy"`
	Codex    Codex     `yaml:"codex"`
	Desktop  Desktop   `yaml:"desktop"`
	Memcap   Memcap    `yaml:"memcap"`
	Run      RunConfig `yaml:"run"`
	Lanes    []Lane    `yaml:"lanes"`
	Merge    Merge     `yaml:"merge"`
	// Units configures the transient user services beekeeper starts.
	Units  Units  `yaml:"units"`
	Alerts Alerts `yaml:"alerts"`
	// Upgrades paces the reading of the installations' Cluster API
	// upgrades.
	Upgrades Upgrades `yaml:"upgrades"`
	Notify   Notify   `yaml:"notify"`
	Metrics  Metrics  `yaml:"metrics"`
	Ollama   Ollama   `yaml:"ollama"`
	// Lemonade is the host's Lemonade Server, a second model server under
	// the same model-server lease and ollama's budgets.
	Lemonade Lemonade `yaml:"lemonade"`
	// Supervisor configures the supervisor loop.
	Supervisor Supervisor `yaml:"supervisor"`
	// Guide is the role that walks the person through the decisions
	// waiting on them.
	Guide Guide `yaml:"guide"`
	// Agents configures how a registered agent near its context limit is
	// handed over to a fresh session.
	Agents Agents `yaml:"agents"`
	// Capacity is how many agents the supervisor keeps busy and the
	// headroom a new start needs.
	Capacity Capacity `yaml:"capacity"`
	// Stop is what beekeeper stop stops besides beekeeper's own units.
	Stop Stop `yaml:"stop"`
	// Reporter is the one-off status reporter session the standby watch
	// starts once per interval.
	Reporter Reporter `yaml:"reporter"`
	// Doctor is what `beekeeper doctor` and the watch fix by themselves.
	Doctor Doctor `yaml:"doctor"`
	// Outbound is what the hook refuses to let leave the machine and where
	// the watch looks for credentials left exposed on disk.
	Outbound Outbound `yaml:"outbound"`
	// Hooks says where beekeeper's Claude Code hooks act.
	Hooks Hooks `yaml:"hooks"`
	// Scan is where the transcript value scanner's index takes its values
	// from.
	Scan Scan `yaml:"scan"`
	// Secret configures beekeeper secret.
	Secret Secret `yaml:"secret"`
	// Sandbox is the agent sandbox beekeeper renders into Claude Code's
	// managed settings (beekeeper sandbox).
	Sandbox Sandbox `yaml:"sandbox"`
	// Board is the project board `beekeeper board` picks work from.
	Board Board `yaml:"board"`
	// Apps is where the organisation declares the GitHub Apps whose
	// consent `beekeeper app allow` records.
	Apps Apps `yaml:"apps"`
	// Plans are the repositories whose pull requests a note for a person
	// names only once their stage check is green, and where an epic's plan
	// pull request is found.
	Plans Plans `yaml:"plans"`
	// Central is the central instance (beekeeper serve behind muster) that
	// holds the leases of the shared installations and the roster of every
	// machine's agents; without its context, all of it stays on the machine.
	Central Central `yaml:"central"`
	// Feedback is the Slack feedback watch: the person's replies in the
	// threads of the reporter's posts, read through muster as the person.
	Feedback Feedback `yaml:"feedback"`
	// Sink dispatches a GitHub workflow on selected events.
	Sink Sink `yaml:"sink"`
}

// Stop configures beekeeper stop.
type Stop struct {
	// Units are more user units (a timer of the desk's own) that beekeeper
	// stop stops while they run and beekeeper resume starts again.
	Units []string `yaml:"units"`
}

// Overlaps tunes which sessions count as working on the same thing.
type Overlaps struct {
	// Ignore lists repositories whose issues and name form no overlap:
	// issue trackers and notebooks every session writes to.
	Ignore []string `yaml:"ignore"`
	// ActiveWithin leaves out sessions idle for longer.
	ActiveWithin Duration `yaml:"activeWithin"`
}

// Duration is a time.Duration written as "30s", "10m" in YAML.
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string, or whole days as "7d".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			d.Duration = time.Duration(n) * 24 * time.Hour
			return nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

// Tokens is a token count written as "400k", "1m" or "400000" in YAML.
type Tokens int64

// UnmarshalYAML parses a count with an optional k or m suffix.
func (t *Tokens) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseTokens(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*t = v
	return nil
}

// ParseTokens parses "400k", "1m" or "400000" (case-insensitive).
func ParseTokens(s string) (Tokens, error) {
	num, mult := strings.ToLower(strings.TrimSpace(s)), 1.0
	switch {
	case strings.HasSuffix(num, "k"):
		num, mult = strings.TrimSuffix(num, "k"), 1e3
	case strings.HasSuffix(num, "m"):
		num, mult = strings.TrimSuffix(num, "m"), 1e6
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || !(f*mult >= 1 && f*mult < 1e12) {
		return 0, fmt.Errorf("%q is not a positive token count (400k, 1m, 400000)", s)
	}
	return Tokens(f * mult), nil
}

// Path returns the configuration file beekeeper reads.
func Path(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv("BEEKEEPER_CONFIG"); env != "" {
		if _, err := os.Stat(filepath.Clean(env)); err != nil { //nolint:gosec // the configuration file explicitly selected by the caller
			return "", err
		}
		return env, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "beekeeper", "config.yaml"), nil
}

// Load reads the file at path and applies the defaults. A configuration
// that is only incomplete loads: Incomplete names what is missing. A missing
// file explicitly named by BEEKEEPER_CONFIG is an error, never defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if env := os.Getenv("BEEKEEPER_CONFIG"); env != "" && filepath.Clean(env) == filepath.Clean(path) {
			return nil, err
		}
		raw = nil
	case err != nil:
		return nil, err
	}
	return parse(path, raw)
}

// parse is the configuration raw holds, read from path, with the defaults
// applied and validated.
func parse(path string, raw []byte) (*Config, error) {
	c := &Config{Supervisor: Supervisor{HostsFirst: true}}
	if err := yaml.Unmarshal(raw, c); err != nil {
		var doc yaml.Node
		if yaml.Unmarshal(raw, &doc) == nil && len(doc.Content) > 0 {
			var findings, invalid []string
			root := effectiveNode(doc.Content[0], "", map[*yaml.Node]bool{}, &findings)
			checkNode(root, reflect.TypeFor[Config](), "", &findings, &invalid)
			if len(findings) > 0 {
				return nil, fmt.Errorf("%s: %s: %w", path, strings.Join(findings, "; "), err)
			}
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.defaults(); err != nil {
		return nil, err
	}
	return c, c.validate()
}

// Incomplete names the references whose source section is missing: each
// fails the commands that follow it, and leaves every other command
// working, so an edit that adds a reference and its section one after the
// other never makes the configuration unloadable in between.
func (c *Config) Incomplete() []string {
	var out []string
	for i, id := range c.Secret.AgeIdentities {
		switch {
		case strings.HasPrefix(id.Ref, "store://") && len(c.Secret.Store.Read) == 0:
			out = append(out, fmt.Sprintf("secret.ageIdentities[%d]: ref %q: a store:// reference takes secret.store.read", i, id.Ref))
		case id.Ref == "store://" && len(c.Secret.Store.Search) == 0:
			out = append(out, fmt.Sprintf("secret.ageIdentities[%d]: ref %q: store:// without an entry takes secret.store.search", i, id.Ref))
		}
	}
	return out
}
