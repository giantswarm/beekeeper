package board

import (
	"cmp"
	"fmt"
	"strings"
	"testing"
)

// gqlType is a type of GitHub's GraphQL schema as the board's queries meet
// it: its fields with the type each answers ("" for one the check does not
// follow). A complete type lists every field GitHub declares on it, so a
// selection of any other is the error GitHub answers; the interfaces and
// unions are complete, the object types are listed only for the way down
// to them.
type gqlType struct {
	complete bool
	fields   map[string]string
}

// githubSchemaText is the part of GitHub's public GraphQL schema the
// board's queries pass through, one type a line: its kind, its name and
// its fields, each with the type it answers where the check follows it.
// The interfaces and unions are complete, a query selects any other field
// of theirs only through an inline fragment of a concrete type; the object
// types name only the fields that lead to them.
const githubSchemaText = `
type Query repositoryOwner:RepositoryOwner repository:Repository search:SearchResultItemConnection node:Node
type Mutation addProjectV2ItemById:AddProjectV2ItemByIdPayload updateProjectV2ItemPosition:UpdateProjectV2ItemPositionPayload
type AddProjectV2ItemByIdPayload item:ProjectV2Item
type UpdateProjectV2ItemPositionPayload items:ProjectV2ItemConnection
type SearchResultItemConnection nodes:SearchResultItem
type Repository issue:Issue issueOrPullRequest:IssueOrPullRequest
type Issue projectItems:ProjectV2ItemConnection
type ProjectV2ItemConnection nodes:ProjectV2Item
type ProjectV2Item project:ProjectV2 content:ProjectV2ItemContent fieldValueByName:ProjectV2ItemFieldValue
type ProjectV2 owner:ProjectV2Owner items:ProjectV2ItemConnection fields:ProjectV2FieldConfigurationConnection field:ProjectV2FieldConfiguration
type ProjectV2FieldConfigurationConnection nodes:ProjectV2FieldConfiguration
type ProjectV2ItemFieldNumberValue creator:Actor
type ProjectV2ItemFieldSingleSelectValue creator:Actor
interface ProjectV2Owner id projectV2:ProjectV2 projectsV2
interface RepositoryOwner avatarUrl id login repositories repository:Repository resourcePath url
interface Actor avatarUrl login resourcePath url
interface Node id
union ProjectV2ItemFieldValue
union ProjectV2ItemContent
union ProjectV2FieldConfiguration
union IssueOrPullRequest
union SearchResultItem
`

// githubSchema is githubSchemaText by type name.
var githubSchema = func() map[string]gqlType {
	m := map[string]gqlType{}
	for line := range strings.Lines(githubSchemaText) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		t := gqlType{complete: f[0] != "type", fields: map[string]string{}}
		for _, x := range f[2:] {
			name, typ, _ := strings.Cut(x, ":")
			t.fields[name] = typ
		}
		m[f[1]] = t
	}
	return m
}()

// gqlSel is one selection of a selection set: a field with its own
// selections, an inline fragment on a type ("" for none) or a fragment
// spread.
type gqlSel struct {
	name, on, spread string
	inline           bool
	set              []gqlSel
}

// gqlDoc is a parsed query: its operations by their root type and its
// fragments by name.
type gqlDoc struct {
	ops   []gqlSel
	frags map[string]gqlSel
}

type gqlParser struct {
	s   string
	i   int
	err error
}

func (p *gqlParser) fail(format string, a ...any) {
	if p.err == nil {
		p.err = fmt.Errorf("at %d: %s", p.i, fmt.Sprintf(format, a...))
	}
	p.i = len(p.s)
}

func (p *gqlParser) space() {
	for p.i < len(p.s) && strings.IndexByte(" \t\r\n,", p.s[p.i]) >= 0 {
		p.i++
	}
}

func isNameByte(b byte) bool {
	return b == '_' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}

func (p *gqlParser) name() string {
	p.space()
	j := p.i
	for p.i < len(p.s) && isNameByte(p.s[p.i]) {
		p.i++
	}
	return p.s[j:p.i]
}

func (p *gqlParser) eat(tok string) bool {
	p.space()
	if strings.HasPrefix(p.s[p.i:], tok) {
		p.i += len(tok)
		return true
	}
	return false
}

// args skips a parenthesised argument or variable list, its strings with
// their escapes.
func (p *gqlParser) args() {
	depth := 0
	for ; p.i < len(p.s); p.i++ {
		switch p.s[p.i] {
		case '"':
			for p.i++; p.i < len(p.s) && p.s[p.i] != '"'; p.i++ {
				if p.s[p.i] == '\\' {
					p.i++
				}
			}
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				p.i++
				return
			}
		}
	}
	p.fail("unterminated arguments")
}

func (p *gqlParser) set() []gqlSel {
	if !p.eat("{") {
		p.fail("want a selection set")
		return nil
	}
	out := []gqlSel{}
	for !p.eat("}") {
		if p.i >= len(p.s) {
			p.fail("unterminated selection set")
			return out
		}
		if p.eat("...") {
			switch n := p.name(); n {
			case "on":
				on := p.name()
				out = append(out, gqlSel{inline: true, on: on, set: p.set()})
			case "":
				out = append(out, gqlSel{inline: true, set: p.set()})
			default:
				out = append(out, gqlSel{spread: n})
			}
			continue
		}
		s := gqlSel{name: p.name()}
		if s.name == "" {
			p.fail("unexpected %q", p.s[p.i:min(p.i+10, len(p.s))])
			return out
		}
		if p.eat(":") {
			s.name = p.name()
		}
		if p.space(); p.i < len(p.s) && p.s[p.i] == '(' {
			p.args()
		}
		if p.space(); p.i < len(p.s) && p.s[p.i] == '{' {
			s.set = p.set()
		}
		out = append(out, s)
	}
	return out
}

func parseGQL(q string) (gqlDoc, error) {
	p := &gqlParser{s: q}
	d := gqlDoc{frags: map[string]gqlSel{}}
	for p.space(); p.i < len(p.s); p.space() {
		if p.s[p.i] == '{' {
			d.ops = append(d.ops, gqlSel{on: "Query", set: p.set()})
			continue
		}
		switch kw := p.name(); kw {
		case "query", "mutation":
			p.name()
			if p.space(); p.i < len(p.s) && p.s[p.i] == '(' {
				p.args()
			}
			d.ops = append(d.ops, gqlSel{on: strings.ToUpper(kw[:1]) + kw[1:], set: p.set()})
		case "fragment":
			n := p.name()
			if p.name() != "on" {
				p.fail("fragment %s without a type condition", n)
				continue
			}
			on := p.name()
			d.frags[n] = gqlSel{on: on, set: p.set()}
		default:
			p.fail("unexpected definition %q", kw)
		}
	}
	return d, p.err
}

// checkGQL is the errors GitHub answers to query for selecting a field its
// type does not declare, as far as githubSchema knows the types.
func checkGQL(q string) []string {
	d, err := parseGQL(q)
	if err != nil {
		return []string{"unparsable query: " + err.Error()}
	}
	var errs []string
	var walk func(typ string, set []gqlSel)
	walk = func(typ string, set []gqlSel) {
		t := githubSchema[typ]
		for _, s := range set {
			switch {
			case s.spread != "":
				if _, ok := d.frags[s.spread]; !ok {
					errs = append(errs, fmt.Sprintf("Fragment %s was used, but not defined", s.spread))
				}
			case s.inline:
				walk(cmp.Or(s.on, typ), s.set)
			case s.name == "__typename":
			default:
				child, ok := t.fields[s.name]
				if !ok && t.complete {
					errs = append(errs, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", s.name, typ))
				}
				if s.set != nil {
					walk(child, s.set)
				}
			}
		}
	}
	for _, op := range d.ops {
		walk(op.on, op.set)
	}
	for _, f := range d.frags {
		walk(f.on, f.set)
	}
	return errs
}

// checkSchema fails t for a GraphQL request whose query GitHub rejects for
// a field its type does not declare: every fake of the board's requests
// runs it, so such a query fails here and not against GitHub.
func checkSchema(t *testing.T, args []string) {
	t.Helper()
	if len(args) < 4 || args[0] != "api" || args[1] != "graphql" || !strings.HasPrefix(args[3], "query=") {
		return
	}
	q := strings.TrimPrefix(args[3], "query=")
	for _, e := range checkGQL(q) {
		t.Errorf("GitHub rejects the query: %s\n%s", e, q)
	}
}

func TestCheckGQLRejectsWhatGitHubRejects(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want []string
	}{
		// The item read of a move as it was: ProjectV2Owner declares no login.
		{`query{repository(owner:"o",name:"r"){issue(number:5){id projectItems(first:50){nodes{id project{id number owner{...on ProjectV2Owner{login}}}}}}}}`,
			[]string{"Field 'login' doesn't exist on type 'ProjectV2Owner'"}},
		{`query{repository(owner:"o",name:"r"){issue(number:5){projectItems(first:50){nodes{project{owner{login}}}}}}}`,
			[]string{"Field 'login' doesn't exist on type 'ProjectV2Owner'"}},
		{`query($o:String!){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:1){owner{id}}} title}}`,
			[]string{"Field 'title' doesn't exist on type 'RepositoryOwner'"}},
		{`query{repository(owner:"o",name:"r"){issue(number:5){projectItems(first:1){nodes{s:fieldValueByName(name:"S"){name}}}}}}`,
			[]string{"Field 'name' doesn't exist on type 'ProjectV2ItemFieldValue'"}},
		{`query{repository(owner:"o",name:"r"){issue(number:5){...I}}}`, []string{"Fragment I was used, but not defined"}},
		{`query{repository(owner:"o",name:"r"){issue(number:5){...I}}}` + issueFields, nil},
		{`mutation($p:ID!,$c:ID!){addProjectV2ItemById(input:{projectId:$p,contentId:$c}){item{` + itemFields + `}}}`, nil},
		{`query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){id projectItems(first:50){nodes{
` + itemFields + `}}}}}`, nil},
		{`query{s1:search(query:"repo:o/r \"a\" is:open",type:ISSUE,first:50){nodes{...on Issue{number} __typename}}}`, nil},
	} {
		got := checkGQL(tc.q)
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("checkGQL(%s) = %q, want %q", tc.q, got, tc.want)
		}
	}
}
