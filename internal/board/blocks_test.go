package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
)

// fakeDeps answers a hold's reads from a map of issues and pull requests by
// repository and number, and applies its mutations to the item's blockers.
type fakeDeps struct {
	t *testing.T
	// nodes are the issues and pull requests by "repo#n"; an issue's state
	// is in it, its blockers in blockers.
	nodes     map[string]string
	blockers  map[string][]string
	mutations []string
	repos     []string
	// lag answers the summary as GitHub does for seconds after a mutation:
	// without the blockers its list shows.
	lag bool
}

func (f *fakeDeps) run(ctx context.Context, args ...string) ([]byte, error) {
	checkSelectionSchema(f.t, args)
	q := args[3]
	checkedQuery(f.t, q)
	f.repos = append(f.repos, github.Repo(ctx))
	vars := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vars[k] = v
	}
	open := func(key string) int {
		n := 0
		for _, b := range f.blockers[key] {
			if strings.Contains(f.nodes[b], `"OPEN"`) {
				n++
			}
		}
		return n
	}
	// summary is the issue's blockers, as their list and as the summary
	// older readers used.
	summary := func(key string) string {
		var bs []string
		for _, b := range f.blockers[key] {
			r, n, _ := strings.Cut(b, "#")
			bs = append(bs, fmt.Sprintf(`{"number":%s,"state":%q,"repository":{"nameWithOwner":"o/%s"}}`, n, strings.Trim(strings.TrimPrefix(f.nodes[b], `{"state":`), `"}`), r))
		}
		o, all := open(key), len(f.blockers[key])
		if f.lag {
			o, all = 0, 0
		}
		return fmt.Sprintf(`"issueDependenciesSummary":{"blockedBy":%d,"totalBlockedBy":%d},"blockedBy":{"totalCount":%d,"nodes":[%s]}`,
			o, all, len(f.blockers[key]), strings.Join(bs, ","))
	}
	switch {
	case strings.Contains(q, "addBlockedBy") || strings.Contains(q, "removeBlockedBy"):
		m := "addBlockedBy"
		if strings.Contains(q, "removeBlockedBy") {
			m = "removeBlockedBy"
		}
		item, by := strings.TrimPrefix(vars["i"], "ID:"), strings.TrimPrefix(vars["b"], "ID:")
		f.mutations = append(f.mutations, m+" "+item+" "+by)
		if m == "addBlockedBy" {
			f.blockers[item] = append(f.blockers[item], by)
		} else {
			f.blockers[item] = slices.DeleteFunc(f.blockers[item], func(b string) bool { return b == by })
		}
		return fmt.Appendf(nil, `{"data":{"%s":{"issue":{"id":"ID:%s","state":"OPEN",%s}}}}`, m, item, summary(item)), nil
	case strings.Contains(q, "issueOrPullRequest"):
		key := vars["r"] + "#" + vars["n"]
		node, ok := f.nodes[key]
		if !ok {
			return fmt.Appendf(nil, `{"data":{"repository":{"issueOrPullRequest":null}},"errors":[{"message":"Could not resolve to an issue or pull request with the number of %s."}]}`, vars["n"]), nil
		}
		if strings.Contains(node, "PullRequest") {
			return fmt.Appendf(nil, `{"data":{"repository":{"issueOrPullRequest":%s}}}`, node), nil
		}
		return fmt.Appendf(nil, `{"data":{"repository":{"issueOrPullRequest":{"__typename":"Issue","id":"ID:%s",%s,%s}}}}`,
			key, strings.Trim(node, "{}"), summary(key)), nil
	}
	f.t.Fatalf("unexpected request %v", args)
	return nil, nil
}

// openIssue and closedIssue are an issue's state as fakeDeps keeps it,
// pr7 a pull request.
const (
	openIssue      = `{"state":"OPEN"}`
	closedIssue    = `{"state":"CLOSED"}`
	pr7            = "o/r#7"
	repoR          = "o/r"
	issueQ3        = "o/q#3"
	addBlockedR5Q3 = "addBlockedBy r#5 q#3"
)

func newFakeDeps(t *testing.T) *fakeDeps {
	return &fakeDeps{t: t, blockers: map[string][]string{}, nodes: map[string]string{
		"r#5":  openIssue,
		"r#9":  openIssue,
		"q#3":  openIssue,
		"r#7":  `{"__typename":"PullRequest"}`,
		"r#15": `{"__typename":"PullRequest","closingIssuesReferences":{"nodes":[{"__typename":"Issue","id":"ID:r#5","state":"OPEN","number":5,"repository":{"nameWithOwner":"o/r"},"blockedBy":{"totalCount":0,"nodes":[]}}]}}`,
		"q#13": `{"__typename":"PullRequest","closingIssuesReferences":{"nodes":[{"__typename":"Issue","id":"ID:q#3","state":"OPEN","number":3,"repository":{"nameWithOwner":"o/q"},"blockedBy":{"totalCount":0,"nodes":[]}}]}}`,
	}}
}

func TestBlockResolvesPullRequestsToClosingIssues(t *testing.T) {
	f := newFakeDeps(t)
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7}}
	ctx := context.Background()
	h, err := c.Block(ctx, "o/r#15", "o/q#13")
	if err != nil || *h != (Held{Ref: issue5, By: issueQ3, ByOpen: true, Changed: true, Blockers: 1, OpenBlockers: 1}) ||
		!slices.Equal(f.mutations, []string{addBlockedR5Q3}) {
		t.Fatalf("Block(PR, PR) = %+v, %v; mutations %v", h, err, f.mutations)
	}
}

func TestBlockAndUnblockRecordTheDependency(t *testing.T) {
	f := newFakeDeps(t)
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7}}
	ctx := context.Background()
	h, err := c.Block(ctx, "https://github.com/o/r/issues/5", "o/q#3")
	if err != nil || *h != (Held{Ref: issue5, By: "o/q#3", ByOpen: true, Changed: true, Blockers: 1, OpenBlockers: 1}) ||
		!slices.Equal(f.mutations, []string{"addBlockedBy r#5 q#3"}) {
		t.Fatalf("Block = %+v, %v; mutations %v", h, err, f.mutations)
	}
	// Each issue is read on its own repository, the dependency written on
	// the item's; the board's items are never read.
	if !slices.Equal(f.repos, []string{repoR, "o/q", repoR}) {
		t.Errorf("acting on %q", f.repos)
	}
	// A dependency recorded already is left as it is; the counts are the
	// blockers' list, while the summary still lags.
	f.lag = true
	if h, err := c.Block(ctx, issue5, "o/q#3"); err != nil || h.Changed || h.OpenBlockers != 1 || len(f.mutations) != 1 {
		t.Errorf("Block again = %+v, %v; mutations %v", h, err, f.mutations)
	}
	f.lag = false
	h, err = c.Unblock(ctx, issue5, "https://github.com/o/q/issues/3")
	if err != nil || *h != (Held{Ref: issue5, By: "o/q#3", ByOpen: true, Changed: true}) ||
		!slices.Equal(f.mutations, []string{"addBlockedBy r#5 q#3", "removeBlockedBy r#5 q#3"}) {
		t.Errorf("Unblock = %+v, %v; mutations %v", h, err, f.mutations)
	}
	if h, err := c.Unblock(ctx, issue5, "o/q#3"); err != nil || h.Changed || len(f.mutations) != 2 {
		t.Errorf("Unblock again = %+v, %v; mutations %v", h, err, f.mutations)
	}
}

func TestBlockRefusesWhatIsNoIssue(t *testing.T) {
	f := newFakeDeps(t)
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7}}
	for _, tc := range []struct {
		block     bool
		ref, by   string
		want      string
		wantExact bool
	}{
		{true, issue5, pr7, pr7 + " is a pull request: a blocker is an issue; a hold on a review or a person is the item's Status Blocked / Waiting plus a decision for that person", true},
		{true, issue5, "o/r#404", "o/r#404 is no issue: Could not resolve to an issue or pull request with the number of 404.", true},
		{false, issue5, pr7, pr7 + " is a pull request", false},
		{true, pr7, issue9, pr7 + " is a pull request: a hold is recorded on an issue", true},
		{true, "o/r#404", issue9, "o/r#404 is no issue", false},
		{true, issue5, "https://github.com/o/r/issues/5", "o/r#5 cannot be blocked by itself", true},
	} {
		hold := c.Unblock
		if tc.block {
			hold = c.Block
		}
		_, err := hold(context.Background(), tc.ref, tc.by)
		var r *Refusal
		if !errors.As(err, &r) || (tc.wantExact && r.Reason != tc.want) || !strings.HasPrefix(r.Reason, tc.want) {
			t.Errorf("hold(%s, %s) = %v, want refusal %q", tc.ref, tc.by, err, tc.want)
		}
	}
	if len(f.mutations) != 0 {
		t.Errorf("a refused hold wrote %v", f.mutations)
	}
}

func TestABlockedItemIsSkippedThenOfferedOnceItsBlockerCloses(t *testing.T) {
	f := newFakeDeps(t)
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7}}
	ctx := context.Background()
	b := deskBoard()
	// Keep GitHub's summary stale while the dependency list changes.
	f.lag = true
	// Rank the same issue fragment the board, sub-issues and searches read.
	rank := func() Candidate {
		var r struct {
			Repository struct {
				Item issueJSON `json:"issueOrPullRequest"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issueOrPullRequest(number:$n){...I}}}`+issueFields,
			vars{"o": "o", "r": "r", "n": 5}); err != nil {
			t.Fatal(err)
		}
		it := r.Repository.Item.item()
		it.Ref, it.OnBoard, it.Status, it.Created, it.Updated = issue5, true, upNext, days(5), days(1)
		snap := snapshot(t)
		snap.Items, snap.Search = []Item{it}, nil
		cands := Rank(snap, b, now)
		if len(cands) != 1 {
			t.Fatalf("candidates %+v", cands)
		}
		return cands[0]
	}
	if got := rank(); got.Skip != "" {
		t.Fatalf("unblocked item skipped: %q", got.Skip)
	}
	if _, err := c.Block(ctx, issue5, issue9); err != nil {
		t.Fatal(err)
	}
	if got := rank(); got.Skip != "held: blocked by o/r#9; 1 of 1 blockers open" {
		t.Errorf("blocked item: skip %q, want held: blocked by o/r#9; 1 of 1 blockers open", got.Skip)
	}
	// The blocker closes; no session acts on the item.
	f.nodes["r#9"] = closedIssue
	if got := rank(); got.Skip != "" {
		t.Errorf("item whose blocker closed: skip %q, want offered", got.Skip)
	}
	// Unblock offers an item whose blocker is still open.
	f.nodes["r#9"] = openIssue
	if _, err := c.Unblock(ctx, issue5, issue9); err != nil {
		t.Fatal(err)
	}
	if got := rank(); got.Skip != "" {
		t.Errorf("unblocked item: skip %q, want offered", got.Skip)
	}
}

// blockedMeta is metaJSON with a blocked Status, s4.
var blockedMeta = strings.Replace(metaJSON, `{"id":"s3","name":"In Progress ⛏️"}`, `{"id":"s3","name":"In Progress ⛏️"},{"id":"s4","name":"Blocked / Waiting ⛔️"}`, 1)

// A move to the blocked Status says what holds the item: --by records an
// open issue as its blocker first, an item with an open recorded blocker
// moves as it is, and anything else is refused before anything is written,
// as is --by with a move to another Status or a closed blocker. A move to
// another Status without --by is Move.
func TestMoveHeldRecordsWhatHoldsABlockedItem(t *testing.T) {
	deps := newFakeDeps(t)
	gh := &fakeGH{t: t, item: onBoard}
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		switch q := args[3]; {
		case strings.Contains(q, "fields(first:50)"):
			return []byte(blockedMeta), nil
		case strings.Contains(q, "issueOrPullRequest") || strings.Contains(q, "BlockedBy("):
			return deps.run(ctx, args...)
		}
		return gh.run(ctx, args...)
	}
	c := &Client{GH: run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	ctx := context.Background()
	refusedWith := func(err error, want string) bool {
		var r *Refusal
		return errors.As(err, &r) && strings.Contains(r.Reason, want)
	}
	// No blocker recorded, none named: refused, nothing written.
	if _, _, err := c.MoveHeld(ctx, issue5, "blocked", ""); !refusedWith(err, "o/r#5 has no open recorded blocker: a move to Blocked / Waiting ⛔️ names what holds it, --by") ||
		len(gh.mutations)+len(deps.mutations) != 0 {
		t.Fatalf("unexplained block = %v; mutations %v %v", err, gh.mutations, deps.mutations)
	}
	// A pull request, or a closed issue, holds no item.
	deps.nodes["r#6"] = closedIssue
	for by, want := range map[string]string{pr7: pr7 + " is a pull request", "o/r#6": "o/r#6 is closed: it holds nothing"} {
		if _, _, err := c.MoveHeld(ctx, issue5, "waiting", by); !refusedWith(err, want) || len(gh.mutations)+len(deps.mutations) != 0 {
			t.Errorf("--by %s = %v; mutations %v %v", by, err, gh.mutations, deps.mutations)
		}
	}
	// A PR alias must not bypass the closed-blocker check.
	deps.nodes["q#13"] = strings.ReplaceAll(deps.nodes["q#13"], `"OPEN"`, `"CLOSED"`)
	if _, _, err := c.MoveHeld(ctx, issue5, "blocked", "o/q#13"); !refusedWith(err, "o/q#3 is closed: it holds nothing") || len(gh.mutations)+len(deps.mutations) != 0 {
		t.Fatalf("closed blocker through PR = %v; mutations %v %v", err, gh.mutations, deps.mutations)
	}
	// --by names what holds a blocked item only.
	if _, _, err := c.MoveHeld(ctx, issue5, "progress", issue9); !refusedWith(err, "In Progress ⛏️ is none") || len(gh.mutations)+len(deps.mutations) != 0 {
		t.Errorf("--by with In Progress = %v", err)
	}
	// --by an open issue: recorded, then moved.
	mv, h, err := c.MoveHeld(ctx, issue5, "blocked", issue9)
	if err != nil || mv.To != "Blocked / Waiting ⛔️" || h == nil || !h.Changed || h.OpenBlockers != 1 ||
		!slices.Equal(deps.mutations, []string{"addBlockedBy r#5 r#9"}) || !slices.Equal(gh.mutations, []string{"I1=s4"}) {
		t.Fatalf("MoveHeld --by = %+v, %+v, %v; mutations %v %v", mv, h, err, deps.mutations, gh.mutations)
	}
	// Its blocker recorded, the item moves without --by.
	if _, h, err := c.MoveHeld(ctx, issue5, "blocked", ""); err != nil || h == nil || h.Changed || h.OpenBlockers != 1 || len(gh.mutations) != 2 {
		t.Errorf("MoveHeld with a recorded blocker = %+v, %v; mutations %v", h, err, gh.mutations)
	}
	// Any other move is Move.
	if mv, h, err := c.MoveHeld(ctx, issue5, "up next", ""); err != nil || h != nil || mv.To != upNext {
		t.Errorf("MoveHeld to Up Next = %+v, %+v, %v", mv, h, err)
	}
}
