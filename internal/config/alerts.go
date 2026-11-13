package config

import (
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/alerts"
)

// Alerts configures the reading of the installations' Alertmanagers: watch
// reads them every Every and prints NEW and RESOLVED lines, snapshot adds
// the current set as a section.
type Alerts struct {
	// Installations are read always; the discovered ones (Discover) and a
	// held lease whose name resolves to a kube context are read too.
	Installations []Installation `yaml:"installations"`
	// Ignore are alert names that never appear; setting it replaces the
	// default (Watchdog).
	Ignore []string `yaml:"ignore"`
	// Tenant is the Mimir tenant (X-Scope-OrgID) whose Alertmanager is read
	// first; empty, only a plain Alertmanager is read.
	Tenant string `yaml:"tenant"`
	// Team is the team whose alerts are marked in capitals and counted.
	Team string `yaml:"team"`
	// Collapse is the number of changes of one alertname in one run above
	// which they are one line with a count.
	Collapse int `yaml:"collapse"`
	// Every is the watch's reading interval.
	Every Duration `yaml:"every"`
	// Timeout bounds the reading of one installation, port-forwards included.
	Timeout Duration `yaml:"timeout"`
	// Kubectl is the kubectl binary.
	Kubectl string `yaml:"kubectl"`
	// Kubeconfig lists the kubeconfig files (~/ allowed) the installations'
	// contexts are resolved and read in, passed to kubectl as KUBECONFIG:
	// a kubeconfig of the watch's own, logged into with KUBECONFIG=<file>
	// tsh kube login <cluster>, so an installation reads without a context
	// in the machine kubeconfig. The machine kubeconfig is merged first
	// unless the list names it. Empty, kubectl's own default.
	Kubeconfig []string `yaml:"kubeconfig"`
	// Flap is the flap damper.
	Flap Flap `yaml:"flap"`
	// Quiet are the rules whose alerts' changes are no wake-up: the watch
	// logs them (watch.quiet) instead of printing them. A rule never
	// quiets an alert of the team, one on an installation in play (leased,
	// claimed or merged into within the last half hour, or with a merge
	// settling), or a page unless it names a cluster. Setting it replaces
	// the default: once Team is set, every other team's and team-less
	// notify alert ({severity: notify}), else none. An alert back after a
	// reading that missed it, with its old start, is quiet too.
	Quiet []alerts.Quiet `yaml:"quiet"`
	// PageSeverity is the severity that pages the on-call person (default
	// page): a firing alert at it that no session owns (alerts own) for
	// OwnerGrace is a PAGE UNOWNED line, again every OwnerGrace.
	PageSeverity string `yaml:"pageSeverity"`
	// OwnerGrace is how long a page may go unowned (default 15m).
	OwnerGrace Duration `yaml:"ownerGrace"`
	// PagerDuty is the second alert source: the team's PagerDuty services'
	// open incidents.
	PagerDuty PagerDuty `yaml:"pagerduty"`
	// Discover adds every installation that carries the team's
	// HelmReleases.
	Discover Discover `yaml:"discover"`
}

// Discover finds the installations to read beyond alerts.installations:
// every cluster Teleport lists (tsh kube ls, teleport.tsh on
// teleport.home), but the excluded ones, that carries a HelmRelease
// labelled Label=alerts.team. One without a kube context is logged into the
// first file of alerts.kubeconfig that is not the machine kubeconfig
// (KUBECONFIG=<file> tsh kube login <cluster>), so its reads need no
// context in the person's kubeconfig. The watch discovers every Every and
// keeps the result (alerts-discovery.json in the state directory), which
// every reading of the alerts adds to its installations.
type Discover struct {
	// Every is the discovery's interval; unset, nothing is discovered.
	Every Duration `yaml:"every"`
	// Label is the HelmRelease label naming the team (default
	// application.giantswarm.io/team).
	Label string `yaml:"label"`
	// Exclude are the glob patterns of Teleport cluster names never read
	// (default "*-*", a workload cluster's <installation>-<cluster>, and
	// "*.*", the proxy's own).
	Exclude []string `yaml:"exclude"`
}

// Enabled says whether installations are discovered.
func (d Discover) Enabled() bool { return d.Every.Duration > 0 }

// PagerDuty configures the reading of the open incidents of the team's
// PagerDuty services, through muster's read-only PagerDuty tools as the
// person, signed in to the muster context: beekeeper holds no PagerDuty
// token. The watch reads them every Every and prints PAGERDUTY NEW,
// ACKNOWLEDGED and RESOLVED lines; alerts.team marks them.
type PagerDuty struct {
	// Context is the muster context whose PagerDuty server is read; unset,
	// PagerDuty is not read. The binary and the call timeout are central's.
	Context string `yaml:"context"`
	// Server is the name muster registered the PagerDuty MCP server under:
	// its tools are x_<server>_<tool> (default pd).
	Server string `yaml:"server"`
	// Services are the ids of the team's PagerDuty services.
	Services []string `yaml:"services"`
	// Every is the reading interval (default 1m): a page is a watch line
	// within it.
	Every Duration `yaml:"every"`
}

// Enabled says whether PagerDuty is read.
func (p PagerDuty) Enabled() bool { return p.Context != "" }

// Upgrades paces the reading of alerts.installations' Cluster API clusters
// for running upgrades: each installation is read every Every, and every
// watch.interval while an upgrade runs on it or holds it.
type Upgrades struct {
	// Every is how old an installation's last reading may grow before it is
	// read again (default 5m).
	Every Duration `yaml:"every"`
}

// Flap holds back an alert that changes too often: its Changes-th NEW or
// RESOLVED within Window is one FLAPPING line, as is a re-fire within Window
// after it resolved, and its changes print nothing until it has been stable
// for Window.
type Flap struct {
	Changes int      `yaml:"changes"`
	Window  Duration `yaml:"window"`
}

// Installation is an installation and, optionally, the kube context that
// reaches it and the lowest severity printed of its alerts; written as a
// name alone or as {name, context, floor}. Without a context it is
// kube.contextTemplate's, else <name>, else the one ending in @<name>.
type Installation struct {
	Name    string `yaml:"name"`
	Context string `yaml:"context"`
	// Floor is the lowest severity printed (alerts.Severities: none, info,
	// warning, notify, critical, page); empty prints every alert.
	Floor string `yaml:"floor"`
}

// UnmarshalYAML accepts a plain name.
func (i *Installation) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&i.Name)
	}
	type plain Installation
	return n.Decode((*plain)(i))
}

// OtherTeamsNotify are other teams' and team-less notify alerts: notify
// pages nobody, and the rule never quiets the team's alerts, a page or an
// installation in play.
var OtherTeamsNotify = alerts.Quiet{Severity: "notify"}

// DefaultQuietFor are the default quiet rules of team: OtherTeamsNotify
// once there is a team whose alerts it leaves out, else none.
func DefaultQuietFor(team string) []alerts.Quiet {
	if team == "" {
		return []alerts.Quiet{}
	}
	return []alerts.Quiet{OtherTeamsNotify}
}

// DefaultQuietSessions are the sessions of beekeeper's own tests.
var DefaultQuietSessions = []string{"test: *"}

// DefaultIgnore are the alerts that always fire: Alertmanager's dead man's
// switch.
var DefaultIgnore = []string{"Watchdog"}
