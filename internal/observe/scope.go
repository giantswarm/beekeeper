package observe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// Reasons an Item leaves by scope (item.removed).
const (
	RemovedTeamChanged = "team-changed"
	RemovedArchived    = "archived"
	RemovedDeleted     = "deleted"
	RemovedRetention   = "retention"
)

// Tracks reports whether a board item the engine doesn't track yet belongs to the tracked set of a
// team with board option teamOption: an open issue of the team in an active status, or one of its bugs
// in the Inbox.
func Tracks(bi forge.BoardItem, teamOption string) bool {
	if !bi.IsIssue || bi.Archived || teamOption == "" || bi.Team != teamOption {
		return false
	}
	if IssueState(bi.Issue.State, bi.Issue.StateReason) != v1alpha1.IssueOpen {
		return false
	}
	return IsActive(bi.Status) || (bi.Status == StatusInbox && bi.Kind == KindBug)
}

// LeftScope says why a tracked Item's board item left the team's scope ("" when it is still in scope,
// whatever its status): it was deleted from the board, archived, or moved to another team.
func LeftScope(bi forge.BoardItem, found bool, teamOption string) string {
	switch {
	case !found:
		return RemovedDeleted
	case bi.Archived:
		return RemovedArchived
	case bi.Team != teamOption:
		return RemovedTeamChanged
	}
	return ""
}

// InQuery reports whether the board resync's filtered query should return a tracked item as last
// observed: when it doesn't, the engine reads the item by node id once to learn why.
func InQuery(status, kind, issueState string) bool {
	if issueState != v1alpha1.IssueOpen {
		return false
	}
	return IsActive(status) || (status == StatusInbox && kind == KindBug)
}

// TeamQuery is the board resync's filtered query for a team.
func TeamQuery(org string, project int, teamOption string) forge.TeamItemsQuery {
	return forge.TeamItemsQuery{Org: org, Project: project, TeamOption: teamOption,
		Statuses: append([]string(nil), ActiveStatuses...), InboxStatus: StatusInbox, BugKind: KindBug}
}

// UnknownOptions returns the team options that aren't options of the board's Team field.
func UnknownOptions(schema forge.BoardSchema, teamOptions []string) []string {
	known := map[string]bool{}
	for field, opts := range schema.Options {
		if strings.EqualFold(field, "Team") {
			for _, o := range opts {
				known[o] = true
			}
		}
	}
	var unknown []string
	for _, o := range teamOptions {
		if !known[o] {
			unknown = append(unknown, o)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// Backoff is the transient retry delay before attempt n (0-based): initial, doubling, capped.
func Backoff(initial, cap time.Duration, n int) time.Duration {
	if initial <= 0 {
		initial = 10 * time.Second
	}
	if cap <= 0 {
		cap = 5 * time.Minute
	}
	d := initial
	for i := 0; i < n && d < cap; i++ {
		d *= 2
	}
	return min(d, cap)
}

// Inputs are what a refused read depends on: the installation's repos, the configuration, and the
// webhooks seen for a repo or issue. A refused read is retried only when its inputs differ. Webhooks are
// counted only for the keys a standing refusal watches, so the counts stay bounded.
type Inputs struct {
	mu        sync.Mutex
	org       string
	repos     map[string]bool
	reposHash string
	config    string
	webhooks  map[string]int
}

// NewInputs returns inputs for an org's installation.
func NewInputs(org string) *Inputs { return &Inputs{org: org, webhooks: map[string]int{}} }

// SetOrg sets the installation's org (repos of the org count as installed until the repos are known).
func (in *Inputs) SetOrg(org string) {
	in.mu.Lock()
	in.org = org
	in.mu.Unlock()
}

// SetRepos records the installation's repos.
func (in *Inputs) SetRepos(repos []string) {
	sorted := sortedCopy(repos)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	in.mu.Lock()
	defer in.mu.Unlock()
	in.repos = map[string]bool{}
	for _, r := range sorted {
		in.repos[strings.ToLower(r)] = true
	}
	in.reposHash = hex.EncodeToString(sum[:8])
}

// SetConfig records the configuration's version (e.g. the Installation's generation and the Teams').
func (in *Inputs) SetConfig(v string) {
	in.mu.Lock()
	in.config = v
	in.mu.Unlock()
}

// Webhook records a delivery for a repo or an issue or PR ref; keys no refusal watches are ignored.
func (in *Inputs) Webhook(keys ...string) {
	in.mu.Lock()
	for _, k := range keys {
		if _, ok := in.webhooks[k]; ok {
			in.webhooks[k]++
		}
	}
	in.mu.Unlock()
}

// Watch starts counting the webhooks for keys (a repo, a ref): a refusal now depends on them.
func (in *Inputs) Watch(keys ...string) {
	in.mu.Lock()
	for _, k := range keys {
		if _, ok := in.webhooks[k]; !ok && k != "" {
			in.webhooks[k] = 0
		}
	}
	in.mu.Unlock()
}

// Retain stops counting the webhooks for the keys keep rejects (no standing refusal depends on them).
func (in *Inputs) Retain(keep func(key string) bool) {
	in.mu.Lock()
	for k := range in.webhooks {
		if !keep(k) {
			delete(in.webhooks, k)
		}
	}
	in.mu.Unlock()
}

// Installed reports whether repo is covered by the installation: listed, or of the org while the
// listing is unknown.
func (in *Inputs) Installed(repo string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.repos != nil {
		return in.repos[strings.ToLower(repo)]
	}
	owner, _, _ := strings.Cut(repo, "/")
	return in.org != "" && strings.EqualFold(owner, in.org)
}

// Key is the inputs of a read about ref (owner/repo#n) in repo.
func (in *Inputs) Key(repo, ref string) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return fmt.Sprintf("repos=%s;config=%s;repo=%d;ref=%d", in.reposHash, in.config, in.webhooks[repo], in.webhooks[ref])
}
