package config

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// Guide configures the guide: its role and the person it guides.
type Guide struct {
	Role `yaml:",inline"`
	// Person is the name the guide's notes are filed for (note add --for,
	// or an older note's "[for <person>]" prefix), matched without regard
	// to case. Its queue and feed show only that person's notes; empty,
	// they show every note filed --for anyone.
	Person string `yaml:"person"`
	// WaitingTTL is how long a stopped session that waits on the person
	// stays in the queue and the feed; older ones fold into one line.
	WaitingTTL Duration `yaml:"waitingTTL"`
	// PersonNames are other names the person goes by: note add --for one of
	// them files the note for Person, where the guide sees it.
	PersonNames []string `yaml:"personNames"`
	// Desk is the folder (~/ allowed) the guide keeps its memory in, a desk
	// checkout: the guide's session writes files there and the desk commits
	// them. A refused write to a harness's own memory names it. Empty: the
	// guide edits nothing.
	Desk string `yaml:"desk"`
}

// DefaultLoopGraphQLLimit is supervisor.graphQLLimit when unset.
const DefaultLoopGraphQLLimit = 1500

// Supervisor configures the supervisor, which is the supervisor loop: the
// user service beekeeper install always installs. No session holds a
// supervisor role.
type Supervisor struct {
	// ReviewModel overrides reviewer model selection. The review command
	// defaults to opus, the strongest Claude review model alias.
	ReviewModel string `yaml:"reviewModel"`
	// GraphQLLimit is the GraphQL points an hour the loop's board reads may
	// spend of their window (1500): past it the board waits for the reset,
	// while pull reads, reviews and merges go on. A busy desk raises it.
	GraphQLLimit int `yaml:"graphQLLimit"`
	// ReviewLightLines selects diff-only review below this changed-line count.
	// Zero defaults to 150; negative disables the size shortcut.
	ReviewLightLines int `yaml:"reviewLightLines"`

	// Effort is the default for sessions started by the supervisor loop.
	Effort string `yaml:"effort"`
	// ReviewRounds no longer adds review rounds (default 1, the only
	// behavior): a pull request gets one desk review
	// (https://github.com/giantswarm/beekeeper/issues/3861). After its
	// verdict a green head with fixes gets the merge go with no new review:
	// fixes pushed after a review need no review of their own. A head that
	// only brings the base into the findings' head carries no fixes and goes
	// back to rework; one that only brings the base into an approved head (a
	// merge, a conflict's resolution, a rebase) stays covered by that
	// approval.
	ReviewRounds int `yaml:"reviewRounds"`
	// ReviewBrief and WorkerBrief are files (~/ allowed) holding the Go
	// templates of the briefs the loop starts a reviewer and the next
	// worker with (.URL, .Repo, .Number, .Head, .Round, .Worker). Empty:
	// a built-in review brief, and no worker start.
	ReviewBrief string `yaml:"reviewBrief"`
	WorkerBrief string `yaml:"workerBrief"`
	// Hosts are execution host selectors (agents.hosts) the loop's worker
	// starts, and agents start or emergency start without --host, may be
	// placed on, in order of preference. HostsFirst defaults to true. A
	// host must map the item's repository and pass its own check
	// (capacity --host); the start is pinned to the repository's
	// default branch. Reviewers follow the same preferences unless a local
	// review directory is required; releases and the loop stay local.
	Hosts []string `yaml:"hosts"`
	// HostsFirst prefers execution hosts and falls back to the coordinator
	// within its ordinary capacity guards. Defaults to true; explicit false
	// retains local-first placement.
	HostsFirst bool `yaml:"hostsFirst"`
	// PostInstall is the command the loop runs after it installed a
	// release; empty: none.
	PostInstall []string `yaml:"postInstall"`
	// Classifier is the command that classifies a report or park the
	// loop's rules cannot: the event as JSON on stdin, {"class",
	// "confidence"} on stdout. Empty: none, every such event goes to the
	// guide unclassified, as one below ClassifierConfidence (default 0.8).
	Classifier           []string `yaml:"classifier"`
	ClassifierConfidence float64  `yaml:"classifierConfidence"`
	// LoopInterval is the loop's pass (30s), ReleaseEvery how often it
	// looks for a release (10m), NoteDue the due time of the decisions it
	// files for the guide's person (24h).
	LoopInterval Duration `yaml:"loopInterval"`
	ReleaseEvery Duration `yaml:"releaseEvery"`
	// RestartWait bounds how long the loop's restart into a replaced binary
	// waits for its claims in flight (10m), its passes running the merge
	// decisions meanwhile; past it the loop re-executes with them in flight.
	RestartWait Duration `yaml:"restartWait"`
	// PostMergeWindow bounds inactivity while awaiting a post-merge report (2h).
	PostMergeWindow Duration `yaml:"postMergeWindow"`
	NoteDue         Duration `yaml:"noteDue"`
	// FillRetryEvery bounds repeated empty or failed board reads while
	// capacity's ceiling stays unchanged and no agent is done or removed
	// (5m), doubled for each further one in a row up to four times.
	FillRetryEvery Duration `yaml:"fillRetryEvery"`
	// ReserveFor is how long after its step began a review-queued or
	// fixing delivery still reserves a start slot ahead of new work (1h);
	// an older one is a stalled delivery, not a slot held from new starts.
	ReserveFor Duration `yaml:"reserveFor"`
	// UnmergedLimit holds back new board starts on a repository once it has
	// this many open pull requests opened within UnmergedWindow, escalating
	// the backlog to the guide (default 10; negative disables).
	UnmergedLimit int `yaml:"unmergedLimit"`
	// UnmergedWindow is the lookback window for UnmergedLimit (default 168h / 7d).
	UnmergedWindow Duration `yaml:"unmergedWindow"`
	// Ownership is whose pull requests the loop reviews and gives the
	// merge go for.
	Ownership Ownership `yaml:"ownership"`
}

const (
	// DefaultUnmergedLimit is supervisor.unmergedLimit when unset.
	DefaultUnmergedLimit = 10
	// DefaultUnmergedWindow is supervisor.unmergedWindow when unset.
	DefaultUnmergedWindow = 7 * 24 * time.Hour
	// DefaultReserveFor is supervisor.reserveFor when unset.
	DefaultReserveFor = time.Hour
	// DefaultRestartWait is supervisor.restartWait when unset.
	DefaultRestartWait = 10 * time.Minute
)

// DefaultTeamFiles is supervisor.ownership.teamFiles unless the
// configuration sets it: the team files of giantswarm/github.
const DefaultTeamFiles = "giantswarm/github/repositories"

// Ownership is a desk's team ownership: which pull requests its supervisor
// loop reviews and gives the merge go for. Outside github.allowedOrgs it
// never acts, whatever this says.
type Ownership struct {
	// Team is the desk's team as the team files name it (team-bumblebee);
	// empty, every repository of github.allowedOrgs is the desk's.
	Team string `yaml:"team"`
	// TeamFiles is where the organisation declares which team owns a
	// repository, <owner>/<repo>/<dir> (default DefaultTeamFiles, once Team
	// is set): every <team>.yaml directly in <dir> lists that team's
	// repositories of <owner> as `- name: <repo>` entries.
	TeamFiles string `yaml:"teamFiles"`
	// Review are the repositories (owner/name, path.Match patterns such as
	// giantswarm/*-configs) the desk reviews and merges whoever owns them.
	Review []string `yaml:"review"`
	// Never are the repositories (patterns as Review's) the desk does no
	// work on, whatever Review or a person's override says: a pull request
	// in one is left alone. References to these repositories in another
	// pull request do not affect its ownership.
	Never []string `yaml:"never"`
	// Authors are the logins the desk opens its pull requests as (default:
	// the login gh acts as): one anybody else opened is left alone.
	Authors []string `yaml:"authors"`
	// Bots are the bot logins (renovate, dependabot) whose pull requests
	// the loop reviews and gives the go for like the desk's own: the sweep
	// lists them in the repositories the desk owns and reviews one without
	// a live owner, or a worker parks on one for desk review. Their reviews
	// count for nothing, and Never, the team rule and the gate still apply.
	Bots []string `yaml:"bots"`
	// OwnerWait is how long a worker parked on another team's pull request
	// waits for that team's review before the loop asks guide.person to
	// override the ownership of that one pull request (24h).
	OwnerWait Duration `yaml:"ownerWait"`
}

// TeamFilesRepo is TeamFiles' repository (owner/repo) and directory.
func (o Ownership) TeamFilesRepo() (repo, dir string) {
	owner, rest, _ := strings.Cut(o.TeamFiles, "/")
	name, dir, _ := strings.Cut(rest, "/")
	return owner + "/" + name, dir
}

// validateEach reports a team-files place without a directory and a pattern
// path.Match cannot read.
func (o Ownership) validateEach(report func(error)) {
	if o.TeamFiles != "" {
		if repo, dir := o.TeamFilesRepo(); dir == "" || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
			report(fmt.Errorf("teamFiles: %q is not <owner>/<repo>/<dir>", o.TeamFiles))
		}
	}
	for field, patterns := range map[string][]string{"review": o.Review, "never": o.Never} {
		for _, p := range patterns {
			if _, err := path.Match(p, ""); err != nil || !strings.Contains(p, "/") || strings.HasSuffix(p, "/") {
				report(fmt.Errorf("%s: %q is no owner/name pattern", field, p))
			}
		}
	}
}

// Role configures a relayed role, the guide's: the
// successor's session prompt (the instructions it follows, a skill or a
// file, not both) and the relay.
type Role struct {
	// Model overrides the previous holder's model for every successor.
	Model string `yaml:"model"`
	// Effort is every successor's effort level; empty: the harness default.
	Effort string `yaml:"effort"`
	// Skill is the name of the skill the successor runs (guide).
	Skill string `yaml:"skill"`
	// Instructions is a file (~/ allowed) whose content opens the prompt
	// instead.
	Instructions string `yaml:"instructions"`
	// RelayAt is the role's session context, in tokens, at which the watch
	// reports the relay due ("200k" for the guide).
	RelayAt Tokens `yaml:"relayAt"`
	// RelayTTL is how long a relay stays open for the successor's start.
	RelayTTL Duration `yaml:"relayTTL"`
	// RelayGrace is how long a holder that stays over RelayAt has after
	// the relay due to relay itself; past it, the standby watch relays it.
	RelayGrace Duration `yaml:"relayGrace"`
	// RestartGrace is how long beekeeper waits after it first saw the
	// role's CLI gone: a CLI back under the same session within it is a
	// restart and keeps the role; past it, the watch says the holder is
	// gone and starts its successor.
	RestartGrace Duration `yaml:"restartGrace"`
	// Dir is the folder a successor starts in (~/ allowed; default: the
	// folder its predecessor's desktop session started from, not the
	// worktree the desktop made for it, whose branch the desktop cannot
	// check out a second time).
	Dir string `yaml:"dir"`
	// Profile names the tool profile (agents.profiles, or the built-in
	// minimal) of the role's successors and its holder's headless wakes;
	// empty: none, they get the person's whole tool surface.
	Profile string `yaml:"profile"`
}

// defaultRelayAt is the context, in tokens, at which a role's holder or an
// agent is relayed unless configured.
const defaultRelayAt = 400_000

func (r *Role) defaults(home string) {
	setDur(&r.RelayTTL, 15*time.Minute)
	setDur(&r.RelayGrace, 30*time.Minute)
	setDur(&r.RestartGrace, time.Minute)
	if r.RelayAt == 0 {
		r.RelayAt = defaultRelayAt
	}
	r.Instructions = homePath(home, r.Instructions)
	r.Dir = homePath(home, r.Dir)
}
