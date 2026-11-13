package config

import (
	"maps"
	"slices"
)

// Central configures the central instance, reached through muster as the
// person.
type Central struct {
	// Context is the muster context of the installation it runs on
	// (muster context list): its endpoint, and the person's token from
	// muster auth token, taken anew for each call.
	Context string `yaml:"context"`
	// Server is the name muster registered it under: its tools are
	// x_<server>_<tool> (default beekeeper).
	Server string `yaml:"server"`
	// Muster is the muster binary (default muster).
	Muster string `yaml:"muster"`
	// Timeout bounds one call (default 30s).
	Timeout Duration `yaml:"timeout"`
}

// Enabled says whether a central instance is configured.
func (c Central) Enabled() bool { return c.Context != "" }

// CentralLane says whether the lane's merges queue in the central
// instance: its installation is central.
func (c *Config) CentralLane(l Lane) bool {
	return l.Installation != "" && c.IsCentral(l.Installation)
}

// IsCentral says whether res is a central resource: a central instance is
// configured and res is none of the machine's own (Leasable), whose leases
// stay local. The central instance knows which names are its Environments.
func (c *Config) IsCentral(res string) bool {
	return c.Central.Enabled() && res != "" && !c.IsLeasable(res)
}

// Identity names the person, team and host this machine's sessions act
// for.
type Identity struct {
	// Person is the person's verified email, stable across identity
	// providers.
	Person string `yaml:"person"`
	// Team is the person's team.
	Team string `yaml:"team"`
	// Host names the machine (default: its host name, without the domain).
	Host string `yaml:"host"`
}

// Serve configures `beekeeper serve`, the central instance behind muster:
// whose Dex tokens it accepts and what their groups authorize.
// FleetWatch selects the read-only muster tools used by serve's fleet watch.
// Empty tools disables the watch, allowing identity grants to be deployed first.
type FleetWatch struct {
	MinCallInterval Duration `yaml:"minCallInterval"`
	Kubernetes      string   `yaml:"kubernetes"`
	Kagent          string   `yaml:"kagent"`
	Alertmanager    string   `yaml:"alertmanager"`
	Concurrency     int      `yaml:"concurrency"`
	Timeout         Duration `yaml:"timeout"`
}

type Serve struct {
	FleetWatch FleetWatch `yaml:"fleetWatch"`
	// Issuer is the Dex issuer URL every caller's ID token is checked
	// against (its JWKS).
	Issuer string `yaml:"issuer"`
	// ClientIDs are the audiences accepted: the client muster forwards the
	// person's token for, and the one the installations exchange tokens for.
	ClientIDs []string `yaml:"clientIDs"`
	// Organization is the Dex group every member of the organization
	// carries: a caller without it, and without any of Organizations, is
	// refused.
	Organization string `yaml:"organization"`
	// Organizations are further groups that make a caller a member: one per
	// Dex connector where the connectors spell the organization differently
	// (a GitHub org's <connector>:<org>:<team>, a directory's group name).
	// Use the exact token group, e.g. giantswarm-github:giantswarm:employees;
	// an unprefixed <org>:<team> matches only an unprefixed token group.
	Organizations []string `yaml:"organizations"`
	// Teams maps a Dex group to the team its members belong to; a caller
	// in none is refused, a caller in several is the team of TeamGroups'
	// first. A team's decisions are answered by DecidingGroups.
	Teams map[string]string `yaml:"teams"`
	// Supervisors maps a team to the Dex group of its supervisor role, which
	// may release the team's leases and lift its holds.
	Supervisors map[string]string `yaml:"supervisors"`
	// Muster is the URL of the muster (its /mcp) a send_message to a
	// kagent: address goes through, as the caller.
	Muster string `yaml:"muster"`
	// Kagent maps an installation to the muster tool of its kagent MCP
	// server's invoke_agent_instance, x_<server>_invoke_agent_instance: the
	// installations a kagent: address may name.
	Kagent map[string]string `yaml:"kagent"`
	// People maps the name a decision is filed for (note add --for <name>)
	// to the person's email: who may answer it, and whom Slack messages.
	// A --for that is an email needs no entry.
	People map[string]string `yaml:"people"`
	// Channels maps a team to the Slack channel ID its decisions (--for
	// team:<name>) are posted to; any member of the team (its Slack group in
	// groups, else its Dex group in teams) may answer.
	Channels map[string]string `yaml:"channels"`
	// Groups maps a team to the handle of its Slack user group
	// (team-bumblebee for @team-bumblebee), for a team whose Dex group others
	// share (an organization-wide group): only the group's members answer its
	// decisions, as klaus-gateway finds them by their email. A team without
	// an entry is its Dex group's.
	Groups map[string]string `yaml:"groups"`
	// Gateway is the klaus-gateway that puts decisions to their addressee
	// in Slack; without its URL, decisions stay in beekeeper.
	Gateway Gateway `yaml:"gateway"`
	// Identity is serve's own identity toward muster, for the calls no
	// person makes (the fleet watch's reads).
	Identity ServeIdentity `yaml:"identity"`
}

// ServeIdentity is serve's own Dex identity: its projected ServiceAccount
// token exchanged at serve.issuer's Dex (RFC 8693) for a short-lived Dex ID
// token, the only token serve presents to muster as itself. muster accepts
// no ServiceAccount token; Dex is the one trust point.
type ServeIdentity struct {
	// TokenFile is the projected ServiceAccount token (the audience Dex's
	// connector expects), read anew for each exchange.
	TokenFile string `yaml:"tokenFile"`
	// ConnectorID is the Dex connector that trusts the cluster's
	// ServiceAccount issuer.
	ConnectorID string `yaml:"connectorID"`
	// ClientID is the public Dex client the exchange is made as.
	ClientID string `yaml:"clientID"`
	// Audience is the Dex client the issued token is for, requested with
	// Dex's cross-client scope (that client trusts ClientID as a peer);
	// empty, the token is for ClientID.
	Audience string `yaml:"audience"`
}

// OrganizationGroups is every group that makes a caller a member:
// Organization, then Organizations.
func (s Serve) OrganizationGroups() []string {
	var out []string
	if s.Organization != "" {
		out = append(out, s.Organization)
	}
	return append(out, s.Organizations...)
}

// TeamGroups are the groups in Teams, sorted, the most specific first: a
// team's own groups before the organization-wide ones, so a member of both
// a team's group and the organization's is the team's.
func (s Serve) TeamGroups() []string {
	org := s.OrganizationGroups()
	var own, wide []string
	for _, g := range slices.Sorted(maps.Keys(s.Teams)) {
		if slices.Contains(org, g) {
			wide = append(wide, g)
		} else {
			own = append(own, g)
		}
	}
	return append(own, wide...)
}

// DecidingGroups are the groups whose members answer team's decisions: its
// own groups in Teams, or, for a team that has none, the organization-wide
// groups mapped to it. A team with its own group is never answered by
// everyone in the organization.
func (s Serve) DecidingGroups(team string) []string {
	org := s.OrganizationGroups()
	var own, wide []string
	for _, g := range s.TeamGroups() {
		switch {
		case s.Teams[g] != team:
		case slices.Contains(org, g):
			wide = append(wide, g)
		default:
			own = append(own, g)
		}
	}
	if len(own) > 0 {
		return own
	}
	return wide
}

// Gateway configures the decisions' delivery through klaus-gateway's
// POST /decisions.
type Gateway struct {
	// URL is klaus-gateway's base URL.
	URL string `yaml:"url"`
	// TokenFile is the projected ServiceAccount token (audience
	// klaus-gateway) every request carries, read anew for each.
	TokenFile string `yaml:"tokenFile"`
	// AnswerTool is the muster tool a click calls as the person who
	// answers: beekeeper's note_answer behind muster.
	AnswerTool string `yaml:"answerTool"`
	// SendTool is the muster tool a person's reply in a conversation with
	// one of their agents calls as the person: beekeeper's send_message
	// behind muster, to the agent's local: address.
	SendTool string `yaml:"sendTool"`
}
