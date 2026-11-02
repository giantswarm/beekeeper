package board

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// fragmentDef matches a fragment definition a query carries after its
// operation: its name and where its selection set opens.
var fragmentDef = regexp.MustCompile(`fragment\s+(\w+)\s+on\s+\w+\s*\{`)

// spread matches a named fragment spread, never an inline ...on.
var spread = regexp.MustCompile(`\.\.\.(\w+)`)

// connectionSize matches a connection's page size among a field's arguments.
var connectionSize = regexp.MustCompile(`\b(?:first|last)\s*:\s*(\d+)`)

// graphqlCost is the rate limit points GitHub charges for query, by its
// documented formula: every connection costs one request per page its
// parents can reach (each parent at its first or last), the sum divided
// by 100 and rounded, at least 1 point.
// https://docs.github.com/en/graphql/overview/rate-limits-and-node-limits-for-the-graphql-api
func graphqlCost(t *testing.T, query string) int {
	t.Helper()
	q := expandFragments(t, query)
	requests := 0
	stack := []int{1}
	pending := 1 // the multiplier the next selection set opens with
	for i := 0; i < len(q); i++ {
		switch q[i] {
		case '"':
			i = skipString(q, i)
		case '(':
			end := closing(q, i, '(', ')')
			if m := connectionSize.FindStringSubmatch(stripStrings(q[i:end])); m != nil {
				n, _ := strconv.Atoi(m[1])
				requests += stack[len(stack)-1]
				pending = stack[len(stack)-1] * n
			}
			i = end
		case '{':
			stack = append(stack, pending)
			pending = stack[len(stack)-1]
		case '}':
			stack = stack[:len(stack)-1]
			pending = stack[len(stack)-1]
		case ' ', '\n', '\t':
		default:
			if !isName(q[i]) {
				continue
			}
			// A new field: what opens next is under the current set.
			j := i
			for j < len(q) && isName(q[j]) {
				j++
			}
			pending = stack[len(stack)-1]
			i = j - 1
		}
	}
	return max(1, int(math.Round(float64(requests)/100)))
}

func isName(b byte) bool {
	return b == '_' || b == '$' || b == ':' || b == '.' || b == '!' || b == ',' ||
		('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// skipString is the index of the quote closing the string opening at i.
func skipString(q string, i int) int {
	for j := i + 1; j < len(q); j++ {
		switch q[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return len(q) - 1
}

// stripStrings blanks the string literals of s.
func stripStrings(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			i = skipString(s, i)
			b.WriteString(`""`)
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// closing is the index of the bracket closing the one opening at i.
func closing(q string, i int, open, close byte) int {
	depth := 0
	for j := i; j < len(q); j++ {
		switch q[j] {
		case '"':
			j = skipString(q, j)
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(q) - 1
}

// expandFragments inlines the named fragments query defines into its
// operation and drops their definitions.
func expandFragments(t *testing.T, query string) string {
	t.Helper()
	frags := map[string]string{}
	op := query
	for {
		m := fragmentDef.FindStringSubmatchIndex(op)
		if m == nil {
			break
		}
		end := closing(op, m[1]-1, '{', '}')
		frags[op[m[2]:m[3]]] = op[m[1]:end]
		op = op[:m[0]] + op[end+1:]
	}
	for range 5 {
		op = spread.ReplaceAllStringFunc(op, func(s string) string {
			if body, ok := frags[s[3:]]; ok {
				return body
			}
			return s
		})
	}
	return op
}

func TestGraphQLCostFollowsGitHubsFormula(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		// GitHub's own example: 1 + 100 + 5000 requests.
		{`query{viewer{login repositories(first:100){edges{node{id issues(first:50){edges{node{id labels(first:60){edges{node{id name}}}}}}}}}}}`, 51},
		{`query{viewer{login}}`, 1},
		{`query{a:search(query:"x {first:500}",first:10){nodes{...on PullRequest{comments(last:50){nodes{body}} commits(last:100){nodes{id}}}}}}`, 1},
		{`query{repository(owner:"o",name:"r"){issue(number:1){subIssues(first:50){nodes{...I}}}}}fragment I on Issue{labels(first:20){nodes{name}} assignees(first:10){nodes{login}}}`, 1},
		{`query($c:String){o{items(first:100,after:$c){nodes{content{...I}}}}}fragment I on Issue{labels(first:20){nodes{name}} assignees(first:10){nodes{login}} blockedBy(first:20){nodes{number}}}`, 3},
	} {
		if got := graphqlCost(t, tc.query); got != tc.want {
			t.Errorf("cost %d, want %d: %s", got, tc.want, tc.query)
		}
	}
}

// largeBoard is a board the size the desk's walks met: epics in progress
// with their sub-issues, epics in planning, ranked epics and a long tail
// of items, read by an order with a step of every Status (so the read takes
// every open item of the team), sub-issues, a search and plans.
type largeBoard struct {
	t        *testing.T
	progress int // epics in progress, each with subs sub-issues, open of them open
	planning int // epics in Up Next
	tail     int // plain items
	subs     int
	open     int
	ranked   int // the epics in progress with a rank
	queries  []string
	cost     int
}

const largeMeta = `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{},
{"id":"FS","name":"Status","options":[{"id":"s0","name":"Inbox 📥"},{"id":"s1","name":"Backlog 📦"},{"id":"s2","name":"Up Next ➡️"},{"id":"s3","name":"In Progress ⛏️"}]},
{"id":"FK","name":"Kind","options":[{"id":"k1","name":"Epic 🎯"},{"id":"k2","name":"Alert 🚨"}]},
{"id":"FT","name":"Team","options":[{"id":"t1","name":"X 🐝"}]},
{"id":"FP","name":"Priority","dataType":"NUMBER"}]}}}}}`

func (b *largeBoard) order() config.Board {
	return config.Board{Owner: "o", Project: 7, Team: "X", People: []string{"me"}, Order: []config.BoardStep{
		{Name: "page", Kind: []string{"alert"}},
		{Name: "In Progress", Status: []string{"in progress"}, SubIssues: true},
		{Name: "Up Next", Status: []string{"up next"}},
		{Name: "queue", Search: "repo:o/q"},
		{Name: "Backlog", Status: []string{"backlog"}},
	}}
}

// items are the board's open items of the team, in its order.
func (b *largeBoard) items() []string {
	var out []string
	n := 1
	for i := range b.progress {
		prio := "null"
		if i < b.ranked {
			prio = fmt.Sprintf(`{"number":%d}`, i+1)
		}
		out = append(out, fmt.Sprintf(`{"id":"I%d","status":{"name":%q},"kind":{"name":"Epic 🎯"},"priority":%s,"content":%s}`,
			n, inProgress, prio, issue(n, fmt.Sprintf(`,"subIssuesSummary":{"total":%d,"completed":%d}`, b.subs, b.subs-b.open))))
		n++
	}
	for range b.planning {
		out = append(out, fmt.Sprintf(`{"id":"I%d","status":{"name":%q},"kind":{"name":"Epic 🎯"},"content":%s}`, n, upNext, issue(n, "")))
		n++
	}
	for i := range b.tail {
		st := []string{backlog, upNext}[i%2]
		out = append(out, fmt.Sprintf(`{"id":"I%d","status":{"name":%q},"kind":null,"content":%s}`, n, st, issue(n, "")))
		n++
	}
	return out
}

var (
	epicAlias   = regexp.MustCompile(`e(\d+):repository\(owner:"o",name:"r"\)\{issue\(number:(\d+)\)\{subIssues\(first:(\d+)\)`)
	threadAlias = regexp.MustCompile(`e(\d+):repository\(owner:"o",name:"r"\)\{isPrivate`)
	searchAlias = regexp.MustCompile(`([sp]\d+):search\(`)
)

func (b *largeBoard) run(_ context.Context, args ...string) ([]byte, error) {
	q := strings.TrimPrefix(args[3], "query=")
	b.queries = append(b.queries, q)
	b.cost += graphqlCost(b.t, q)
	vars := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vars[k] = v
	}
	switch {
	case strings.Contains(q, "fields(first:50)"):
		return []byte(largeMeta), nil
	case strings.Contains(q, "items(first:100"):
		all := b.items()
		if strings.Contains(vars["q"], "kind:") {
			all = all[:b.progress+b.planning]
		}
		page, _ := strconv.Atoi(vars["c"])
		end := min(len(all), page+100)
		next := ""
		if end < len(all) {
			next = fmt.Sprintf(`"hasNextPage":true,"endCursor":"%d"`, end)
		} else {
			next = `"hasNextPage":false`
		}
		return fmt.Appendf(nil, `{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{%s},"nodes":[%s]}}}}}`, next, strings.Join(all[page:end], ",")), nil
	case strings.Contains(q, "subIssues("):
		var parts []string
		for _, m := range epicAlias.FindAllStringSubmatch(q, -1) {
			epicN, _ := strconv.Atoi(m[2])
			first, _ := strconv.Atoi(m[3])
			var nodes []string
			for k := range min(first, b.subs) {
				n := 10000 + epicN*100 + k
				sub := issue(n, `,"projectItems":{"nodes":[{"project":{"id":"P"},"status":{"name":"In Progress ⛏️"}}]}`)
				if k < b.subs-b.open {
					sub = strings.Replace(sub, "OPEN", "CLOSED", 1)
				}
				nodes = append(nodes, sub)
			}
			parts = append(parts, fmt.Sprintf(`"e%s":{"issue":{"subIssues":{"nodes":[%s]}}}`, m[1], strings.Join(nodes, ",")))
		}
		for _, m := range searchAlias.FindAllStringSubmatch(q, -1) {
			parts = append(parts, fmt.Sprintf(`%q:{"nodes":[%s]}`, m[1], issue(9000, "")))
		}
		return fmt.Appendf(nil, `{"data":{%s}}`, strings.Join(parts, ",")), nil
	case strings.Contains(q, "is:pr"):
		var parts []string
		for _, m := range searchAlias.FindAllStringSubmatch(q, -1) {
			parts = append(parts, fmt.Sprintf(`%q:{"nodes":[]}`, m[1]))
		}
		return fmt.Appendf(nil, `{"data":{%s}}`, strings.Join(parts, ",")), nil
	case strings.Contains(q, "isPrivate issue("):
		var parts []string
		for _, m := range threadAlias.FindAllStringSubmatch(q, -1) {
			parts = append(parts, fmt.Sprintf(`"e%s":{"isPrivate":false,"issue":{"comments":{"nodes":[]}}}`, m[1]))
		}
		return fmt.Appendf(nil, `{"data":{%s}}`, strings.Join(parts, ",")), nil
	}
	b.t.Fatalf("unexpected request %s", q)
	return nil, nil
}

// The GraphQL cost of one board read over a large order, by GitHub's
// formula: the walk of board next reads it once. Each epic's sub-issues are
// read by a page of its own size: a page of 50 for every epic cost this
// board of 426 candidates 92 points, 53 of them for sub-issue slots no epic
// filled; it costs 39.
func TestReadOfALargeBoardCostsLittleGraphQL(t *testing.T) {
	b := &largeBoard{t: t, progress: 40, planning: 10, tail: 255, subs: 6, open: 4, ranked: 20}
	c := &Client{GH: b.run, Board: b.order(), Plans: []string{plansRepo}}
	snap, err := c.Read(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	cands := Rank(snap, b.order(), now)
	t.Logf("%d candidates: %d requests, %d GraphQL points", len(cands), len(b.queries), b.cost)
	if len(cands) < 426 {
		t.Fatalf("the fixture offers %d candidates, want a large order", len(cands))
	}
	subs := 0
	for _, it := range snap.Items {
		subs += len(it.SubIssues)
	}
	if subs != b.progress*b.open {
		t.Errorf("read %d open sub-issues, want %d", subs, b.progress*b.open)
	}
	for _, q := range b.queries {
		for _, m := range epicAlias.FindAllStringSubmatch(q, -1) {
			if m[3] != strconv.Itoa(b.subs) {
				t.Errorf("epic o/r#%s read by a page of %s sub-issues, it has %d", m[2], m[3], b.subs)
			}
		}
	}
	if b.cost > 40 {
		t.Errorf("one read costs %d GraphQL points, want at most 40", b.cost)
	}
}

// An epic with more sub-issues than one page holds is read by a full page.
func TestSubIssuePageIsTheEpicsSizeUpToItsLimit(t *testing.T) {
	b := &largeBoard{t: t, progress: 2, subs: 120, open: 120}
	c := &Client{GH: b.run, Board: b.order()}
	if _, err := c.Read(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, q := range b.queries {
		for _, m := range epicAlias.FindAllStringSubmatch(q, -1) {
			pages++
			if m[3] != strconv.Itoa(subIssuesRead) {
				t.Errorf("epic o/r#%s read by a page of %s, want %d", m[2], m[3], subIssuesRead)
			}
		}
	}
	if pages != 2 {
		t.Errorf("%d epic pages read, want 2", pages)
	}
}
