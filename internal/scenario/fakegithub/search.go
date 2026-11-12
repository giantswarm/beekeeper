//go:build scenario

package fakegithub

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// GitHub's issue search, as far as beekeeper and gh write it: qualifiers
// (is:, repo:, org:, author:, label:, draft:, head:, base:, state:, …),
// free text against title and body, a leading dash, and in the advanced
// syntax AND, OR and parentheses. Several repo:, org: or user: qualifiers
// side by side are alternatives, as GitHub reads them.

// searchNode is a node of a parsed search: a term, or a combination.
type searchNode struct {
	op   string // "term", "and", "or" or "not"
	term filterTerm
	kids []*searchNode
}

// searchTokens splits a search into parentheses, operators and terms.
func searchTokens(q string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case quoted:
			cur.WriteRune(r)
		case r == '(' || r == ')':
			flush()
			out = append(out, string(r))
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

type searchParser struct {
	toks []string
	pos  int
	sort string
}

func (p *searchParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *searchParser) or() *searchNode {
	left := p.and()
	for p.peek() == "OR" {
		p.pos++
		left = &searchNode{op: "or", kids: []*searchNode{left, p.and()}}
	}
	return left
}

func (p *searchParser) and() *searchNode {
	n := &searchNode{op: "and"}
	for p.pos < len(p.toks) {
		t := p.peek()
		if t == ")" || t == "OR" {
			break
		}
		if t == "AND" {
			p.pos++
			continue
		}
		if kid := p.unary(); kid != nil {
			n.kids = append(n.kids, kid)
		}
	}
	return n
}

func (p *searchParser) unary() *searchNode {
	t := p.peek()
	p.pos++
	switch t {
	case "NOT":
		return &searchNode{op: "not", kids: []*searchNode{p.unary()}}
	case "(":
		n := p.or()
		if p.peek() == ")" {
			p.pos++
		}
		return n
	}
	terms := splitFilter(t)
	if len(terms) == 0 {
		return nil
	}
	term := terms[0]
	if term.Key == "sort" {
		p.sort = strings.Join(term.Values, ",")
		return nil
	}
	node := &searchNode{op: "term", term: term}
	if term.Negated {
		node.term.Negated = false
		return &searchNode{op: "not", kids: []*searchNode{node}}
	}
	return node
}

// hit is an issue or a pull request as a search reads it.
type hit struct {
	issue *Issue
	pull  *Pull
}

func (h hit) repo() *Repo {
	if h.issue != nil {
		return h.issue.Repo
	}
	return h.pull.Repo
}

func (h hit) created() time.Time {
	if h.issue != nil {
		return h.issue.CreatedAt
	}
	return h.pull.CreatedAt
}

func (h hit) updated() time.Time {
	if h.issue != nil {
		return h.issue.UpdatedAt
	}
	return h.pull.UpdatedAt
}

func (h hit) number() int {
	if h.issue != nil {
		return h.issue.Number
	}
	return h.pull.Number
}

// scopeKeys are the qualifiers that are alternatives side by side.
var scopeKeys = []string{"repo", "org", "user", "owner"}

func (m *Model) searchEval(n *searchNode, h hit) (bool, error) {
	switch n.op {
	case "or":
		for _, k := range n.kids {
			ok, err := m.searchEval(k, h)
			if err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	case "not":
		ok, err := m.searchEval(n.kids[0], h)
		return !ok, err
	case "and":
		scoped, inScope := false, false
		for _, k := range n.kids {
			ok, err := m.searchEval(k, h)
			if err != nil {
				return false, err
			}
			if k.op == "term" && slices.Contains(scopeKeys, k.term.Key) {
				scoped = true
				inScope = inScope || ok
				continue
			}
			if !ok {
				return false, nil
			}
		}
		return !scoped || inScope, nil
	}
	return m.searchTerm(n.term, h)
}

var wordRE = regexp.MustCompile(`^\w+$`)

func textMatches(text, term string) bool {
	text, term = strings.ToLower(text), strings.ToLower(term)
	if !wordRE.MatchString(term) {
		return strings.Contains(text, term)
	}
	re, err := regexp.Compile(`(^|\W)` + regexp.QuoteMeta(term) + `($|\W)`)
	return err == nil && re.MatchString(text)
}

func (m *Model) searchTerm(t filterTerm, h hit) (bool, error) {
	state, draft := "", false
	var labels, assignees []string
	var author, title, body, head, base string
	if h.issue != nil {
		i := h.issue
		state, labels, assignees, author, title, body = i.State, i.Labels, i.Assignees, i.Author, i.Title, i.Body
	} else {
		p := h.pull
		state, draft, labels, assignees, author, title, body, head, base = p.State, p.Draft, p.Labels, p.Assignees, p.Author, p.Title, p.Body, p.Head, p.Base
	}
	r := h.repo()
	anyOf := func(match func(v string) bool) bool { return slices.ContainsFunc(t.Values, match) }
	login := func(v string) string {
		if v == "@me" {
			return m.Viewer
		}
		return strings.TrimPrefix(v, "app/")
	}
	switch t.Key {
	case "":
		return textMatches(title+"\n"+body, t.Values[0]), nil
	case "is", "type", "state":
		for _, v := range t.Values {
			switch strings.ToLower(v) {
			case "issue":
				if h.issue != nil {
					return true, nil
				}
			case "pr", "pull-request":
				if h.pull != nil {
					return true, nil
				}
			case "open":
				if state == stateOpen {
					return true, nil
				}
			case "closed":
				if state != stateOpen {
					return true, nil
				}
			case "merged":
				if state == stateMerged {
					return true, nil
				}
			case "unmerged":
				if h.pull != nil && state != stateMerged {
					return true, nil
				}
			case "draft":
				if draft {
					return true, nil
				}
			case "public":
				if !r.Private {
					return true, nil
				}
			case "private":
				if r.Private {
					return true, nil
				}
			default:
				return false, unsupported("the search qualifier %s:%s", t.Key, v)
			}
		}
		return false, nil
	case "repo":
		return anyOf(func(v string) bool { return strings.EqualFold(v, r.Full()) }), nil
	case "org", "user", "owner":
		return anyOf(func(v string) bool { return strings.EqualFold(v, r.Owner) }), nil
	case "author":
		return anyOf(func(v string) bool {
			return strings.EqualFold(login(v), author) || strings.EqualFold(login(v)+"[bot]", author)
		}), nil
	case "assignee":
		return anyOf(func(v string) bool { return containsFold(assignees, login(v)) }), nil
	case "label":
		return anyOf(func(v string) bool { return containsFold(labels, v) }), nil
	case "draft":
		return anyOf(func(v string) bool { return (strings.ToLower(v) == "true") == draft }) && h.pull != nil, nil
	case "head":
		return h.pull != nil && anyOf(func(v string) bool { return v == head }), nil
	case "base":
		return h.pull != nil && anyOf(func(v string) bool { return v == base }), nil
	case "no":
		return anyOf(func(v string) bool {
			switch strings.ToLower(v) {
			case "label":
				return len(labels) == 0
			case "assignee":
				return len(assignees) == 0
			}
			return false
		}), nil
	case "linked":
		return anyOf(func(v string) bool {
			switch {
			case v == "pr" && h.issue != nil:
				return len(m.ClosedByPulls(h.issue)) > 0
			case v == "issue" && h.pull != nil:
				return len(m.Closes(h.pull)) > 0
			}
			return false
		}), nil
	case "in", "archived":
		return true, nil
	}
	return false, unsupported("the search qualifier %s:", t.Key)
}

// Search finds the issues and pull requests a query matches, in its sort
// order (created, newest first, by default).
func (m *Model) Search(query string, _ bool) ([]any, error) {
	p := &searchParser{toks: searchTokens(query)}
	root := p.or()
	var hits []hit
	for _, r := range m.Repos() {
		for _, i := range r.Issues {
			hits = append(hits, hit{issue: i})
		}
		for _, pr := range r.Pulls {
			hits = append(hits, hit{pull: pr})
		}
	}
	var matched []hit
	for _, h := range hits {
		ok, err := m.searchEval(root, h)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, h)
		}
	}
	field, dir, _ := strings.Cut(cmpOr(p.sort, "created-desc"), "-")
	at := hit.created
	if field == "updated" {
		at = hit.updated
	} else if field != "created" {
		return nil, unsupported("the search order sort:%s", p.sort)
	}
	slices.SortStableFunc(matched, func(a, b hit) int {
		c := at(a).Compare(at(b))
		if c == 0 {
			c = a.number() - b.number()
		}
		if dir != "asc" {
			return -c
		}
		return c
	})
	out := make([]any, 0, len(matched))
	for _, h := range matched {
		if h.issue != nil {
			out = append(out, h.issue)
		} else {
			out = append(out, h.pull)
		}
	}
	return out, nil
}
