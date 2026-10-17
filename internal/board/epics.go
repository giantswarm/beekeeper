package board

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
)

// The epic gate's values unless board.epics names others.
const (
	defaultEpicKind = "epic"
	defaultPlanning = "up next"
	defaultGo       = "in progress"
)

// Gate is the epic gate (config.EpicGate) in the board's canonical values;
// the zero Gate is off.
type Gate struct {
	Kind     []string
	Planning []string
	Go       string
}

// Epic reports whether the board item it is an epic.
func (g Gate) Epic(it Item) bool { return it.OnBoard && slices.Contains(g.Kind, it.Kind) }

// InPlanning reports whether it is an epic in planning.
func (g Gate) InPlanning(it Item) bool { return g.Epic(it) && slices.Contains(g.Planning, it.Status) }

// watched reports whether it is an epic in planning or past its go, whose
// plan board next shows.
func (g Gate) watched(it Item) bool {
	return g.InPlanning(it) || (g.Epic(it) && g.Go != "" && it.Status == g.Go)
}

// statuses are the statuses of the epics the gate watches.
func (g Gate) statuses() []string {
	if g.Go == "" {
		return g.Planning
	}
	return append(slices.Clone(g.Planning), g.Go)
}

// ResolveGate resolves e against the board's statuses and kinds. A value
// board.epics names must be the board's; a default the board has not turns
// the gate off (no epic kind, no planning status) or the go unrecorded.
func ResolveGate(e config.EpicGate, statuses, kinds []string) (Gate, error) {
	resolve := func(field string, values, set []string, def string) ([]string, error) {
		if len(set) == 0 {
			if v, err := Resolve(field, values, def); err == nil {
				return []string{v}, nil
			}
			return nil, nil
		}
		out := make([]string, len(set))
		for i, v := range set {
			r, err := Resolve(field, values, v)
			if err != nil {
				return nil, fmt.Errorf("board.epics: %w", err)
			}
			out[i] = r
		}
		return out, nil
	}
	var g Gate
	var err error
	if g.Kind, err = resolve(KindField, kinds, e.Kind, defaultEpicKind); err != nil {
		return Gate{}, err
	}
	if g.Planning, err = resolve(StatusField, statuses, e.Planning, defaultPlanning); err != nil {
		return Gate{}, err
	}
	var gos []string
	if e.Go != "" {
		gos = []string{e.Go}
	}
	if gos, err = resolve(StatusField, statuses, gos, defaultGo); err != nil {
		return Gate{}, err
	}
	if len(g.Kind) == 0 || len(g.Planning) == 0 {
		return Gate{}, nil
	}
	if len(gos) > 0 {
		g.Go = gos[0]
	}
	return g, nil
}

// planning says why the gate holds the item it: an epic in planning, or an
// issue under one, whose nearest epic among its parents on the board is in
// planning; "" when the gate holds it not.
func (snap *Snapshot) planning(it Item, byRef map[string]Item) string {
	g := snap.Gate
	if g.InPlanning(it) {
		if p := snap.Plans[strings.ToLower(it.Ref)]; p != nil {
			return fmt.Sprintf("epic in planning (%s), plan %s (%s): its sub-issues wait for the go, board move %s %q", it.Status, p.URL, p.State, it.Ref, g.Go)
		}
		return fmt.Sprintf("epic in planning (%s), no plan pull request names it: its planning session's", it.Status)
	}
	seen := map[string]bool{}
	for ref := strings.ToLower(it.Parent); ref != "" && !seen[ref]; {
		seen[ref] = true
		p, ok := byRef[ref]
		if !ok {
			return ""
		}
		if g.Epic(p) {
			if g.InPlanning(p) {
				return fmt.Sprintf("sub-issue of epic %s, in planning (%s)", p.Ref, p.Status)
			}
			return ""
		}
		ref = strings.ToLower(p.Parent)
	}
	return ""
}

// Plan is an epic's plan: the open or merged pull request of a plans
// repository whose description names the epic.
type Plan struct {
	Ref   string `json:"ref"`
	URL   string `json:"url"`
	Title string `json:"title"`
	// State is draft, open or merged; Head the head commit.
	State   string `json:"state"`
	Head    string `json:"head"`
	Private bool   `json:"private"`
	id      string
}

// Epic is an epic the gate watches, in planning or past its go, with its
// plan, nil when no plan pull request names it.
type Epic struct {
	Ref    string `json:"ref"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Plan   *Plan  `json:"plan"`
}

// PlanText is the plan in a few words: its URL, its state and its head.
func (e Epic) PlanText() string {
	if e.Plan == nil {
		return "no plan pull request names it"
	}
	return fmt.Sprintf("plan %s (%s, head %s)", e.Plan.URL, e.Plan.State, short(e.Plan.Head))
}

func short(sha string) string { return sha[:min(len(sha), 7)] }

// Epics are the snapshot's epics in planning or past their go, in the
// board's order, each with its plan.
func (snap *Snapshot) Epics() []Epic {
	var out []Epic
	for _, it := range snap.Items {
		if snap.Gate.watched(it) {
			out = append(out, Epic{Ref: it.Ref, URL: it.URL, Title: it.Title, Status: it.Status, Plan: snap.Plans[strings.ToLower(it.Ref)]})
		}
	}
	return out
}

// Epics reads the board's epics in planning or past their go, the team's,
// with their plans: one request for the fields, one per 100 items, one per
// plansBatch epics. Nil when the gate is off.
func (c *Client) Epics(ctx context.Context) ([]Epic, error) {
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	g, err := ResolveGate(c.Board.Epics, m.Fields[StatusField].names(), m.Fields[KindField].names())
	if err != nil || len(g.Kind) == 0 {
		return nil, err
	}
	snap := &Snapshot{Gate: g}
	if snap.Items, err = c.items(ctx, filter(c.Board, nil, g.statuses()...)); err != nil {
		return nil, err
	}
	snap.Items = slices.DeleteFunc(snap.Items, func(it Item) bool { return !g.watched(it) })
	if snap.Plans, err = c.plansOf(ctx, snap); err != nil {
		return nil, err
	}
	return snap.Epics(), nil
}

// plansOf reads the plans of the snapshot's watched epics.
func (c *Client) plansOf(ctx context.Context, snap *Snapshot) (map[string]*Plan, error) {
	var refs []string
	for _, it := range snap.Items {
		if snap.Gate.watched(it) {
			refs = append(refs, it.Ref)
		}
	}
	return c.FindPlans(ctx, refs)
}

// plansBatch is how many epics one request of FindPlans asks about.
const plansBatch = 20

// FindPlans finds the plan of each epic of refs, keyed by the lower-cased
// ref: one search of the plans repositories (Client.Plans) per epic for
// pull requests naming it, plansBatch searches per request, acting on the
// first plans repository. Of those whose description's Epic line names the
// epic (its URL or owner/repo#n), the most recently updated open one is the plan,
// else the most recently updated merged one; a closed one never is. Nil
// when no plans repository is configured.
func (c *Client) FindPlans(ctx context.Context, refs []string) (map[string]*Plan, error) {
	if len(c.Plans) == 0 || len(refs) == 0 {
		return nil, nil
	}
	ctx = github.ActingOn(ctx, c.Plans[0])
	repos := make([]string, len(c.Plans))
	for i, r := range c.Plans {
		repos[i] = "repo:" + r
	}
	out := map[string]*Plan{}
	for batch := range slices.Chunk(refs, plansBatch) {
		var q strings.Builder
		for i, ref := range batch {
			owner, repo, n, err := Ref(ref)
			if err != nil {
				return nil, err
			}
			query := fmt.Sprintf("%s is:pr %q sort:updated-desc", strings.Join(repos, " "), fmt.Sprintf("%s/%s/issues/%d", owner, repo, n))
			fmt.Fprintf(&q, "p%d:search(query:%s,type:ISSUE,first:10){nodes{...on PullRequest{id number url title state isDraft merged headRefOid body repository{nameWithOwner isPrivate}}}}", i, quote(query))
		}
		var r map[string]struct {
			Nodes []struct {
				ID         string `json:"id"`
				Number     int    `json:"number"`
				URL        string `json:"url"`
				Title      string `json:"title"`
				State      string `json:"state"`
				IsDraft    bool   `json:"isDraft"`
				Merged     bool   `json:"merged"`
				Head       string `json:"headRefOid"`
				Body       string `json:"body"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
					IsPrivate     bool   `json:"isPrivate"`
				} `json:"repository"`
			} `json:"nodes"`
		}
		if err := c.graphql(ctx, &r, "query{"+q.String()+"}", nil); err != nil {
			return nil, fmt.Errorf("the epics' plan pull requests: %w", err)
		}
		for i, ref := range batch {
			names := naming(ref)
			var open, merged *Plan
			for _, n := range r[fmt.Sprintf("p%d", i)].Nodes {
				if n.URL == "" || !names.MatchString(n.Body) {
					continue
				}
				p := &Plan{Ref: fmt.Sprintf("%s#%d", n.Repository.NameWithOwner, n.Number), URL: n.URL, Title: n.Title, Head: n.Head, Private: n.Repository.IsPrivate, id: n.ID}
				switch {
				case n.State == stateOpen && open == nil:
					p.State = "open"
					if n.IsDraft {
						p.State = "draft"
					}
					open = p
				case n.Merged && merged == nil:
					p.State = "merged"
					merged = p
				}
			}
			if p := cmp.Or(open, merged); p != nil {
				out[strings.ToLower(ref)] = p
			}
		}
	}
	return out, nil
}

// naming matches a description whose Epic line names the epic ref
// ("Epic: <URL>", "**Epic:** [owner/repo#n](<URL>)"), no digit after the number: a
// plan that lists the issue as a sub-issue is not its plan.
func naming(ref string) *regexp.Regexp {
	owner, repo, n, _ := Ref(ref)
	o, r := regexp.QuoteMeta(owner), regexp.QuoteMeta(repo)
	return regexp.MustCompile(fmt.Sprintf(`(?im)^[\s*_>#-]*epic[*_]*\s*:[*_]*\s*\[?(?:https://)?(?:github\.com/%s/%s/issues/|%s/%s#)%d(?:\D|$)`, o, r, o, r, n))
}

// Go is the go a move is: an epic moved to the gate's go status. Plan is its
// plan, nil for none, and Thread the private thread the go is recorded on
// (owner/repo#n): the epic itself in a private repository, else its plan
// pull request in a private one.
type Go struct {
	Epic   string `json:"epic"`
	Plan   *Plan  `json:"plan,omitempty"`
	Thread string `json:"thread,omitempty"`
	// Comment is the URL of the comment that records the go.
	Comment  string `json:"comment,omitempty"`
	threadID string
}

// goOf is the go of the move of the epic ref, with its plan and private
// thread; a plan without a private thread is refused, nothing written.
func (c *Client) goOf(ctx context.Context, ref string) (*Go, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	g := &Go{Epic: ref}
	plans, err := c.FindPlans(ctx, []string{ref})
	if err != nil {
		return nil, err
	}
	if g.Plan = plans[strings.ToLower(ref)]; g.Plan == nil {
		return g, nil
	}
	var r struct {
		Repository struct {
			IsPrivate bool `json:"isPrivate"`
			Issue     *struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"repository"`
	}
	if err := c.graphql(github.ActingOn(ctx, owner+"/"+repo), &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){isPrivate issue(number:$n){id}}}`,
		vars{"o": owner, "r": repo, "n": n}); err != nil {
		return nil, err
	}
	switch {
	case r.Repository.IsPrivate && r.Repository.Issue != nil:
		g.Thread, g.threadID = ref, r.Repository.Issue.ID
	case g.Plan.Private:
		g.Thread, g.threadID = g.Plan.Ref, g.Plan.id
	default:
		return nil, &Refusal{fmt.Sprintf("the go of %s has no private thread to be recorded on: %s and its plan %s are public; nothing moved", ref, ref, g.Plan.URL)}
	}
	return g, nil
}

// Text is the comment that records the go, by whom it was moved.
func (g *Go) Text(by, status string) string {
	what := fmt.Sprintf("plan %s", g.Plan.URL)
	if g.Thread == g.Plan.Ref {
		what = "this plan"
	}
	return fmt.Sprintf("**Go:** %s moved to %s by %s. Its open sub-issues are offered as work from now on, implemented at %s at its head %s (%s).\n\nWritten by an agent (`beekeeper board move`).",
		g.Epic, status, by, what, g.Plan.Head, g.Plan.State)
}

// RecordGo posts the go's comment on its private thread, acting on the
// thread's repository, and returns the comment's URL.
func (c *Client) RecordGo(ctx context.Context, g *Go, body string) (string, error) {
	return c.comment(ctx, g.Thread, g.threadID, body)
}
