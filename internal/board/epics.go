package board

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

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
func (g Gate) watched(it Item) bool { return g.InPlanning(it) || g.past(it) }

// past reports whether it is an epic past its go.
func (g Gate) past(it Item) bool { return g.Epic(it) && g.Go != "" && it.Status == g.Go }

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
	// State is draft, open or merged, closed only for the plan an epic past
	// its go was implemented at when no other names it; Head the head commit.
	State   string `json:"state"`
	Head    string `json:"head"`
	Private bool   `json:"private"`
	// The go, read for an epic past it from the latest go comment on its
	// private thread: the plan it named (ImplementedPlan, its URL) at the
	// head Implemented, given at GoAt in the comment GoComment. Since says how
	// the plan moved on from it, Moved the commits pushed to it since; ""
	// and 0 when it did not.
	ImplementedPlan string     `json:"implementedPlan,omitempty"`
	Implemented     string     `json:"implemented,omitempty"`
	GoAt            *time.Time `json:"goAt,omitempty"`
	GoComment       string     `json:"goComment,omitempty"`
	Since           string     `json:"since,omitempty"`
	Moved           int        `json:"moved,omitempty"`
	id              string
}

// Epic is an epic the gate watches, in planning or past its go, with its
// plan, nil when no plan pull request names it. NoGo is an epic past its go
// with no go recorded.
type Epic struct {
	Ref    string `json:"ref"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Plan   *Plan  `json:"plan"`
	NoGo   bool   `json:"noGo,omitempty"`
}

// PlanText is the plan in a few words: its URL, its state and its head,
// for an epic past its go the head implemented and how the plan moved on.
func (e Epic) PlanText() string {
	var b strings.Builder
	if e.Plan == nil {
		b.WriteString("no plan pull request names it")
	} else {
		fmt.Fprintf(&b, "plan %s (%s, head %s", e.Plan.URL, e.Plan.State, short(e.Plan.Head))
		if e.Plan.Implemented != "" {
			fmt.Fprintf(&b, ", implemented %s", short(e.Plan.Implemented))
			if e.Plan.ImplementedPlan != e.Plan.URL {
				fmt.Fprintf(&b, " of %s", e.Plan.ImplementedPlan)
			}
		}
		b.WriteString(")")
		if e.Plan.Since != "" {
			b.WriteString(": " + e.Plan.Since)
		}
	}
	if e.NoGo {
		b.WriteString(", no go recorded")
	}
	return b.String()
}

func short(sha string) string { return sha[:min(len(sha), 7)] }

// Epics are the snapshot's epics in planning or past their go, in the
// board's order, each with its plan.
func (snap *Snapshot) Epics() []Epic {
	var out []Epic
	for _, it := range snap.Items {
		if snap.Gate.watched(it) {
			p := snap.Plans[strings.ToLower(it.Ref)]
			out = append(out, Epic{Ref: it.Ref, URL: it.URL, Title: it.Title, Status: it.Status, Plan: p,
				NoGo: snap.Gate.past(it) && (p == nil || p.Implemented == "")})
		}
	}
	return out
}

// Epics reads the board's epics in planning or past their go, the team's,
// with their plans: one request for the fields, one per 100 items, one per
// plansBatch epics, and one per plansBatch epics past their go of one owner
// for their threads. Nil when the gate is off.
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

// plansOf reads the plans of the snapshot's watched epics, for those past
// their go with the go (readGo).
func (c *Client) plansOf(ctx context.Context, snap *Snapshot) (map[string]*Plan, error) {
	var refs, gone []string
	past := map[string]bool{}
	for _, it := range snap.Items {
		if snap.Gate.watched(it) {
			refs = append(refs, it.Ref)
		}
		if snap.Gate.past(it) {
			past[strings.ToLower(it.Ref)] = true
			gone = append(gone, it.Ref)
		}
	}
	prs, err := c.searchPlans(ctx, refs, past)
	if err != nil || prs == nil {
		return nil, err
	}
	threads, err := c.epicThreads(ctx, gone)
	if err != nil {
		return nil, err
	}
	out := map[string]*Plan{}
	for _, ref := range refs {
		l := strings.ToLower(ref)
		p := pick(ref, prs[l])
		if past[l] {
			p = readGo(ref, p, prs[l], threads[l])
		}
		if p != nil {
			out[l] = p
		}
	}
	return out, nil
}

// plansBatch is how many epics one request of FindPlans asks about.
const plansBatch = 20

// The go is read from the last goComments comments of a thread, a plan's
// commits since it from its last goCommits commits.
const (
	goComments = 50
	goCommits  = 100
)

// planPR is a pull request a plans search answers, with its comments and
// commits when it is read for an epic past its go.
type planPR struct {
	ID      string `json:"id"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	State   string `json:"state"`
	IsDraft bool   `json:"isDraft"`
	Merged  bool   `json:"merged"`
	Head    string `json:"headRefOid"`
	Merge   *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Body       string `json:"body"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
		IsPrivate     bool   `json:"isPrivate"`
	} `json:"repository"`
	Comments thread `json:"comments"`
	Commits  struct {
		Nodes []struct {
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// thread is the comments of an issue or pull request.
type thread struct {
	Nodes []struct {
		Body      string    `json:"body"`
		URL       string    `json:"url"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"nodes"`
}

// planMerged is a merged plan's state.
const planMerged = "merged"

// plan is the pull request as an epic's plan, its state the board's word,
// a merged one's head its merge commit.
func (n *planPR) plan() *Plan {
	p := &Plan{Ref: fmt.Sprintf("%s#%d", n.Repository.NameWithOwner, n.Number), URL: n.URL, Title: n.Title, Head: n.Head, Private: n.Repository.IsPrivate, id: n.ID}
	switch {
	case n.Merged:
		p.State = planMerged
		if n.Merge != nil && n.Merge.OID != "" {
			p.Head = n.Merge.OID
		}
	case n.State != stateOpen:
		p.State = "closed"
	case n.IsDraft:
		p.State = "draft"
	default:
		p.State = "open"
	}
	return p
}

// FindPlans finds the plan of each epic of refs, keyed by the lower-cased
// ref: one search of the plans repositories (Client.Plans) per epic for
// pull requests naming it, plansBatch searches per request, acting on the
// first plans repository. Of those whose description's Epic line names the
// epic (its URL or owner/repo#n), the most recently updated open one is the plan,
// else the most recently updated merged one; a closed one never is. Nil
// when no plans repository is configured.
func (c *Client) FindPlans(ctx context.Context, refs []string) (map[string]*Plan, error) {
	prs, err := c.searchPlans(ctx, refs, nil)
	if err != nil || prs == nil {
		return nil, err
	}
	out := map[string]*Plan{}
	for _, ref := range refs {
		if p := pick(ref, prs[strings.ToLower(ref)]); p != nil {
			out[strings.ToLower(ref)] = p
		}
	}
	return out, nil
}

// planFields are the fields of a pull request a plans search reads.
const planFields = "id number url title state isDraft merged headRefOid mergeCommit{oid} body repository{nameWithOwner isPrivate}"

// mergedPlans is how many merged pull requests mergedPlan reads.
const mergedPlans = 30

// mergedPlan is the most recently updated merged pull request of the plans
// repositories whose Epic line names the epic ref, nil for none: one search
// of the merged pull requests carrying the epic's number, which a
// description naming it as owner/repo#n has too where the URL phrase
// FindPlans searches is missing, and which open pull requests mentioning
// the epic do not crowd out.
func (c *Client) mergedPlan(ctx context.Context, ref string) (*Plan, error) {
	if len(c.Plans) == 0 {
		return nil, nil
	}
	_, _, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	repos := make([]string, len(c.Plans))
	for i, r := range c.Plans {
		repos[i] = "repo:" + r
	}
	query := fmt.Sprintf("%s is:pr is:merged %d sort:updated-desc", strings.Join(repos, " "), n)
	var r struct {
		Search struct {
			Nodes []planPR `json:"nodes"`
		} `json:"search"`
	}
	if err := c.graphql(github.ActingOn(ctx, c.Plans[0]), &r, fmt.Sprintf("query{search(query:%s,type:ISSUE,first:%d){nodes{...on PullRequest{%s}}}}", quote(query), mergedPlans, planFields), nil); err != nil {
		return nil, fmt.Errorf("the merged plan pull requests of %s: %w", ref, err)
	}
	return pick(ref, r.Search.Nodes), nil
}

// searchPlans searches the plans repositories for the pull requests naming
// each epic of refs, keyed by the lower-cased ref, most recently updated
// first, with their comments and commits for the epics past (lower-cased)
// names; nil when no plans repository is configured.
func (c *Client) searchPlans(ctx context.Context, refs []string, past map[string]bool) (map[string][]planPR, error) {
	if len(c.Plans) == 0 || len(refs) == 0 {
		return nil, nil
	}
	ctx = github.ActingOn(ctx, c.Plans[0])
	repos := make([]string, len(c.Plans))
	for i, r := range c.Plans {
		repos[i] = "repo:" + r
	}
	out := map[string][]planPR{}
	for batch := range slices.Chunk(refs, plansBatch) {
		var q strings.Builder
		for i, ref := range batch {
			owner, repo, n, err := Ref(ref)
			if err != nil {
				return nil, err
			}
			query := fmt.Sprintf("%s is:pr %q sort:updated-desc", strings.Join(repos, " "), fmt.Sprintf("%s/%s/issues/%d", owner, repo, n))
			var gone string
			if past[strings.ToLower(ref)] {
				gone = fmt.Sprintf(" comments(last:%d){nodes{body url createdAt}} commits(last:%d){nodes{commit{oid}}}", goComments, goCommits)
			}
			fmt.Fprintf(&q, "p%d:search(query:%s,type:ISSUE,first:10){nodes{...on PullRequest{%s%s}}}", i, quote(query), planFields, gone)
		}
		var r map[string]struct {
			Nodes []planPR `json:"nodes"`
		}
		if err := c.graphql(ctx, &r, "query{"+q.String()+"}", nil); err != nil {
			return nil, fmt.Errorf("the epics' plan pull requests: %w", err)
		}
		for i, ref := range batch {
			out[strings.ToLower(ref)] = r[fmt.Sprintf("p%d", i)].Nodes
		}
	}
	return out, nil
}

// pick is the plan of the epic ref among the pull requests its search
// answered (FindPlans), nil for none.
func pick(ref string, prs []planPR) *Plan {
	names := naming(ref)
	var open, merged *Plan
	for i := range prs {
		n := &prs[i]
		if n.URL == "" || !names.MatchString(n.Body) {
			continue
		}
		switch {
		case n.State == stateOpen && open == nil:
			open = n.plan()
		case n.Merged && merged == nil:
			merged = n.plan()
		}
	}
	return cmp.Or(open, merged)
}

// epicThreads reads the comments of each epic of refs in a private
// repository, the go's private thread, keyed by the lower-cased ref,
// plansBatch epics of one owner per request, acting on their repository. An
// epic in a public repository, or one GitHub does not resolve, has none.
func (c *Client) epicThreads(ctx context.Context, refs []string) (map[string]thread, error) {
	out := map[string]thread{}
	for _, group := range github.ByOwner(refs, refRepo) {
		for batch := range slices.Chunk(group, plansBatch) {
			var q strings.Builder
			for i, ref := range batch {
				owner, repo, n, err := Ref(ref)
				if err != nil {
					return nil, err
				}
				fmt.Fprintf(&q, "e%d:repository(owner:%s,name:%s){isPrivate issue(number:%d){comments(last:%d){nodes{body url createdAt}}}}", i, quote(owner), quote(repo), n, goComments)
			}
			var r map[string]*struct {
				IsPrivate bool `json:"isPrivate"`
				Issue     *struct {
					Comments thread `json:"comments"`
				} `json:"issue"`
			}
			if err := c.graphql(github.ActingOn(ctx, refRepo(batch[0])), &r, "query{"+q.String()+"}", nil); err != nil {
				if _, ok := partialOf(err); !ok {
					return nil, fmt.Errorf("the threads of the epics past their go: %w", err)
				}
			}
			for i, ref := range batch {
				if e := r[fmt.Sprintf("e%d", i)]; e != nil && e.IsPrivate && e.Issue != nil {
					out[strings.ToLower(ref)] = e.Issue.Comments
				}
			}
		}
	}
	return out, nil
}

// goLine matches the comment that records a go (Go.Text): the epic, the
// plan it names ("this plan" on the plan's own thread) and its head.
var goLine = regexp.MustCompile(`^\*\*Go:\*\* (\S+) moved to .*? implemented at (?:this plan|plan (\S+)) at its head (\S+) \(`)

// readGo adds to the plan p of the epic ref, past its go, the go: the
// latest go comment on its private thread (epicThreads) or on a private
// plan pull request naming it (prs), a public thread's never, and how the plan moved on since. Without a go comment p is
// returned as it is; with no current plan, the go's plan (closed, or no
// longer naming the epic) is the plan.
func readGo(ref string, p *Plan, prs []planPR, epic thread) *Plan {
	var at time.Time
	var url, plan, head string
	note := func(t thread, thread string) {
		for _, c := range t.Nodes {
			m := goLine.FindStringSubmatch(c.Body)
			if m == nil || !strings.EqualFold(m[1], ref) || cmp.Or(m[2], thread) == "" || !c.CreatedAt.After(at) {
				continue
			}
			at, url, plan, head = c.CreatedAt, c.URL, cmp.Or(m[2], thread), m[3]
		}
	}
	note(epic, "")
	for i := range prs {
		if prs[i].Repository.IsPrivate {
			note(prs[i].Comments, prs[i].URL)
		}
	}
	if at.IsZero() {
		return p
	}
	var of *planPR
	for i := range prs {
		if prs[i].URL == plan {
			of = &prs[i]
			break
		}
	}
	switch {
	case p != nil:
		cp := *p
		p = &cp
	case of != nil:
		p = of.plan()
	default:
		p = &Plan{URL: plan}
	}
	p.ImplementedPlan, p.Implemented, p.GoAt, p.GoComment = plan, head, &at, url
	closed := of != nil && of.State != stateOpen && !of.Merged
	switch {
	case p.URL != plan && closed:
		p.Since = "plan closed, superseded by " + p.URL
	case p.URL != plan:
		p.Since = "superseded by " + p.URL
	case of == nil:
		p.Since = "the plan no longer names the epic"
	case closed:
		p.Since = "plan closed"
	case p.Head != head && (!of.Merged || of.Head != head):
		p.Moved = since(of, head)
		if p.Moved < 0 {
			p.Moved, p.Since = 0, fmt.Sprintf("plan moved since the go: head %s not among its last %d commits", short(head), goCommits)
		} else {
			p.Since = fmt.Sprintf("plan moved since the go: %d commits", p.Moved)
			if p.Moved == 1 {
				p.Since = "plan moved since the go: 1 commit"
			}
		}
	}
	return p
}

// since is how many of the pull request's commits follow head, -1 when
// head is not among them.
func since(pr *planPR, head string) int {
	for i, n := range slices.Backward(pr.Commits.Nodes) {
		if n.Commit.OID == head {
			return len(pr.Commits.Nodes) - 1 - i
		}
	}
	return -1
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
// thread: the plan FindPlans finds, else the merged plan naming the epic
// (mergedPlan), at its merge commit. A plan without a private thread is
// refused, nothing written.
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
		if g.Plan, err = c.mergedPlan(ctx, ref); err != nil || g.Plan == nil {
			return g, err
		}
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
