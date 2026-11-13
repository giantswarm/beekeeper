package config

import (
	"slices"
	"strings"
	"time"
)

// Lane is a set of repositories whose merges roll the same components of an
// installation: one merge at a time, the next once they rolled. A repository
// in no lane is a lane of its own, with no installation to wait for.
type Lane struct {
	Name string `yaml:"name"`
	// Repositories are owner/repo; a bare name matches it under any owner.
	// A repository's name is also the chart name of its HelmReleases.
	Repositories []string `yaml:"repositories"`
	// Installation is where the lane's HelmReleases run; empty: none to wait for.
	Installation string `yaml:"installation"`
	// Context is the kubeconfig context of the installation (default: the
	// one named after it, or ending in -<installation>).
	Context string `yaml:"context"`
}

// DefaultQueueTTL is merge.queueTTL unless the configuration sets it.
const DefaultQueueTTL = 15 * time.Minute

// Merge configures the gate on devctl pr merge.
type Merge struct {
	// RequireReview requires a fixed desk approval covering the merge head;
	// an explicit supervisor go alone cannot substitute for that review.
	RequireReview bool `yaml:"requireReview"`

	// Methods selects squash, rebase or merge for each repository. Empty: squash.
	// Rebase also makes the ordinary devctl gate add --rebase automatically.
	Methods map[string]string `yaml:"methods"`
	// Cap is the most devctl merges and promotes the machine runs when a
	// merge starts; the waits only poll and do not count.
	Cap int `yaml:"cap"`
	// SessionCap is the most merges and promotions one session runs at once:
	// each running devctl polls GitHub, so the rest of its queued merges wait
	// in their lanes without reading it, starting in the order they queued.
	// Negative: no cap.
	SessionCap int `yaml:"sessionCap"`
	// QueueTTL is how long a queued merge keeps its place after its run ended,
	// and how long after it the place holds up the merges behind it.
	QueueTTL Duration `yaml:"queueTTL"`
	// SeedTTL is how long a place queued on a session's behalf is kept from
	// its seeding or the session's last arrival, and a failed run's from the
	// failure.
	SeedTTL Duration `yaml:"seedTTL"`
	// Settle is how long a lane waits after a merge whose release is unknown.
	Settle Duration `yaml:"settle"`
	// SettleTimeout is how long a lane waits for a release to roll before
	// the next merge is refused.
	SettleTimeout Duration `yaml:"settleTimeout"`
	// BudgetFresh is how old the last budget reading may be to gate on.
	BudgetFresh Duration `yaml:"budgetFresh"`
	// ResetFirst is how long after the GitHub budget's reset the merges
	// queued under the floor for it go first: the gated waits, the watch's
	// GitHub timer checks and budget --gate wait for them, and only merges
	// count against Cap for them, the oldest queued first.
	ResetFirst Duration `yaml:"resetFirst"`
	// StallAfter is how long a lane's first arrived merge may wait behind
	// places whose merges are not in the gate before the lane is stalled.
	StallAfter Duration `yaml:"stallAfter"`
	// HungAfter is how long a running merge's devctl may outlive its pull
	// request's merge before the watch ends it: devctl confirms the release
	// within its own --timeout, so one running on past it hangs in its lane.
	HungAfter Duration `yaml:"hungAfter"`
	// CITimeout is devctl pr merge's --timeout, its wait for the CI outcome,
	// when the gated command names none (default 2h, including CI queueing).
	// CITimeouts can override it for repositories with longer pipelines.
	CITimeout Duration `yaml:"ciTimeout"`
	// CITimeouts overrides CITimeout for repositories keyed by owner/repo.
	CITimeouts map[string]Duration `yaml:"ciTimeouts"`
	// BusyBudget is how long a merge with --update-branch keeps its lane
	// place running devctl again after a run ends on a busy base (its base
	// moved during every one of devctl's update waits, exit 3), counted from
	// devctl's first run. Negative: no second run.
	BusyBudget Duration `yaml:"busyBudget"`
	// ToolLimit is how long a foreground gate call runs before it exits 76
	// and leaves the merge to run on outside it, its outcome waking the
	// caller: below a desktop Bash call's 10-minute limit, which stops the
	// call without an exit code. Negative: no limit.
	ToolLimit Duration `yaml:"toolLimit"`
	// DevctlOwners are the owners whose repositories devctl pr merge serves
	// with its configured GitHub authentication; a
	// repository of any other owner, and every repository while it is
	// empty, takes the plain squash merge as the gh login instead.
	DevctlOwners []string `yaml:"devctlOwners"`
	// Admin are the repositories (owner/repo) the desk merges with --admin
	// past their branch ruleset after the approved desk review; only their
	// merge go names --admin.
	Admin []string `yaml:"admin"`
}

// TimeoutFor returns the repository's CI timeout, falling back to CITimeout.
func (m Merge) TimeoutFor(repo string) time.Duration {
	for name, timeout := range m.CITimeouts {
		if strings.EqualFold(name, repo) && timeout.Duration > 0 {
			return timeout.Duration
		}
	}
	return m.CITimeout.Duration
}

// DevctlServes says whether devctl pr merge serves repo's owner.
func (m Merge) DevctlServes(repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	return slices.ContainsFunc(m.DevctlOwners, func(o string) bool { return strings.EqualFold(o, owner) })
}

// AdminMerge says whether the desk merges repo (owner/repo) with --admin
// after the approved desk review.
func (m Merge) AdminMerge(repo string) bool {
	return slices.ContainsFunc(m.Admin, func(r string) bool { return strings.EqualFold(r, repo) })
}

// LaneOf is the lane a repository's merges queue in: the configured lane
// that lists it, else a lane of its own named after it.
func (c *Config) LaneOf(repo string) Lane {
	name := repo[strings.LastIndex(repo, "/")+1:]
	for _, l := range c.Lanes {
		for _, r := range l.Repositories {
			if strings.EqualFold(r, repo) || (!strings.Contains(r, "/") && strings.EqualFold(r, name)) {
				return l
			}
		}
	}
	return Lane{Name: repo, Repositories: []string{repo}}
}

// LaneNamed is the configured lane of that name.
func (c *Config) LaneNamed(name string) (Lane, bool) {
	for _, l := range c.Lanes {
		if l.Name == name {
			return l, true
		}
	}
	return Lane{}, false
}
