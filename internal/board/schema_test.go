package board

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	gqlMutation = "mutation"
	gqlQuery    = "query"
)

// abstractType is an interface or union of GitHub's GraphQL schema: the
// fields a selection on it may take directly, and the object types it
// stands for. A union has no fields but __typename.
type abstractType struct {
	fields   []string
	possible []string
}

// githubSchema is the part of GitHub's public GraphQL schema the board's
// queries reach, one declaration a line:
//
//	interface Name: field ... = ObjectType ...
//	union Name = ObjectType ...
//	Type.field FieldType
//
// The interfaces and unions are the types a selection is checked on: a
// field outside an interface's (a union has none but __typename) is one
// GitHub rejects the whole query for, as it did the owner's login selected
// on ProjectV2Owner (https://github.com/giantswarm/beekeeper/issues/1997).
// The field types lead from a query's root to them; a field missing here
// leaves its selection unchecked up to its next inline fragment.
const githubSchema = `
interface Actor: avatarUrl login resourcePath url = Bot EnterpriseUserAccount Mannequin Organization User
interface ProjectV2Owner: id projectV2 projectsV2 = Issue Organization PullRequest User
interface RepositoryOwner: avatarUrl id login repositories repository resourcePath url = Organization User
union IssueOrPullRequest = Issue PullRequest
union ProjectV2ItemContent = DraftIssue Issue PullRequest
union ProjectV2FieldConfiguration = ProjectV2Field ProjectV2IterationField ProjectV2SingleSelectField
union ProjectV2ItemFieldValue = ProjectV2ItemFieldDateValue ProjectV2ItemFieldIterationValue ProjectV2ItemFieldLabelValue ProjectV2ItemFieldMilestoneValue ProjectV2ItemFieldNumberValue ProjectV2ItemFieldPullRequestValue ProjectV2ItemFieldRepositoryValue ProjectV2ItemFieldReviewerValue ProjectV2ItemFieldSingleSelectValue ProjectV2ItemFieldTextValue ProjectV2ItemFieldUserValue
union SearchResultItem = App Discussion Issue MarketplaceListing Organization PullRequest Repository User
Query.repositoryOwner RepositoryOwner
Query.repository Repository
Query.search SearchResultItemConnection
Mutation.addProjectV2ItemById AddProjectV2ItemByIdPayload
Mutation.archiveProjectV2Item ArchiveProjectV2ItemPayload
Mutation.unarchiveProjectV2Item UnarchiveProjectV2ItemPayload
Mutation.updateProjectV2ItemFieldValue UpdateProjectV2ItemFieldValuePayload
Mutation.clearProjectV2ItemFieldValue ClearProjectV2ItemFieldValuePayload
AddProjectV2ItemByIdPayload.item ProjectV2Item
ArchiveProjectV2ItemPayload.item ProjectV2Item
UnarchiveProjectV2ItemPayload.item ProjectV2Item
UpdateProjectV2ItemFieldValuePayload.projectV2Item ProjectV2Item
ClearProjectV2ItemFieldValuePayload.projectV2Item ProjectV2Item
Repository.issue Issue
Repository.pullRequest PullRequest
Repository.issueOrPullRequest IssueOrPullRequest
Issue.parent Issue
Issue.blockedBy IssueConnection
Issue.projectItems ProjectV2ItemConnection
PullRequest.closingIssuesReferences IssueConnection
PullRequest.projectItems ProjectV2ItemConnection
IssueConnection.nodes Issue
ProjectV2ItemConnection.nodes ProjectV2Item
ProjectV2FieldConfigurationConnection.nodes ProjectV2FieldConfiguration
SearchResultItemConnection.nodes SearchResultItem
ProjectV2Owner.projectV2 ProjectV2
ProjectV2.owner ProjectV2Owner
ProjectV2.items ProjectV2ItemConnection
ProjectV2.fields ProjectV2FieldConfigurationConnection
ProjectV2.field ProjectV2FieldConfiguration
ProjectV2Item.project ProjectV2
ProjectV2Item.content ProjectV2ItemContent
ProjectV2Item.fieldValueByName ProjectV2ItemFieldValue
ProjectV2ItemFieldNumberValue.creator Actor
ProjectV2ItemFieldSingleSelectValue.creator Actor
`

// abstractTypes and fieldTypes are githubSchema's interfaces and unions by
// name and its field types by "Type.field".
var abstractTypes, fieldTypes = parseSchema(githubSchema)

func parseSchema(s string) (map[string]abstractType, map[string]string) {
	abstract, fields := map[string]abstractType{}, map[string]string{}
	for line := range strings.Lines(s) {
		decl, possible, ok := strings.Cut(line, "=")
		w := strings.Fields(decl)
		switch {
		case len(w) == 0:
		case ok && len(w) >= 2 && w[0] == "interface":
			abstract[strings.TrimSuffix(w[1], ":")] = abstractType{fields: w[2:], possible: strings.Fields(possible)}
		case ok && len(w) == 2 && w[0] == "union":
			abstract[w[1]] = abstractType{possible: strings.Fields(possible)}
		case !ok && len(w) == 2:
			fields[w[0]] = w[1]
		default:
			panic("githubSchema: " + line)
		}
	}
	return abstract, fields
}

// gqlTokens splits a GraphQL document into names, punctuators and string
// literals, dropping whitespace, commas and comments.
func gqlTokens(q string) []string {
	var out []string
	for i := 0; i < len(q); {
		switch c := q[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(q) && q[i] != '\n' {
				i++
			}
		case c == '"':
			j := i + 1
			for j < len(q) && q[j] != '"' {
				if q[j] == '\\' {
					j++
				}
				j++
			}
			out = append(out, q[i:min(j+1, len(q))])
			i = j + 1
		case strings.HasPrefix(q[i:], "..."):
			out = append(out, "...")
			i += 3
		case c == '_' || c == '$' || c == '-' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			j := i + 1
			for j < len(q) && (q[j] == '_' || q[j] >= '0' && q[j] <= '9' || q[j] >= 'A' && q[j] <= 'Z' || q[j] >= 'a' && q[j] <= 'z') {
				j++
			}
			out = append(out, q[i:j])
			i = j
		default:
			out = append(out, string(c))
			i++
		}
	}
	return out
}

// checkSchema reports the first selection of q GitHub's schema rejects on
// one of abstractTypes: a field the interface or union does not declare, or
// an inline fragment on a type it cannot be. Selections on object types are
// taken as they are; GitHub's answer to those is in the fakes' data.
func checkSchema(q string) error {
	q = strings.TrimPrefix(q, "query=")
	toks := gqlTokens(q)
	var stack []string // the type of each open selection set, "" unknown
	next := ""         // the type the next "{" opens
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		cur := ""
		if len(stack) > 0 {
			cur = stack[len(stack)-1]
		}
		switch {
		case tok == "(":
			for depth := 0; i < len(toks); i++ {
				if toks[i] == "(" {
					depth++
				} else if toks[i] == ")" {
					if depth--; depth == 0 {
						break
					}
				}
			}
		case tok == "{":
			stack = append(stack, next)
			next = ""
		case tok == "}":
			if len(stack) == 0 {
				return fmt.Errorf("unbalanced } in %s", q)
			}
			stack = stack[:len(stack)-1]
		case len(stack) == 0 && (tok == gqlQuery || tok == gqlMutation):
			next = map[string]string{gqlQuery: "Query", gqlMutation: "Mutation"}[tok]
			if i+1 < len(toks) && toks[i+1] != "(" && toks[i+1] != "{" {
				i++
			}
		case len(stack) == 0 && tok == "fragment":
			if i+3 >= len(toks) || toks[i+2] != "on" {
				return fmt.Errorf("fragment without a type condition in %s", q)
			}
			next = toks[i+3]
			i += 3
		case tok == "...":
			if i+2 < len(toks) && toks[i+1] == "on" {
				t := toks[i+2]
				if err := fragmentOn(cur, t); err != nil {
					return fmt.Errorf("%w in %s", err, q)
				}
				next = t
				i += 2
			} else {
				i++ // a named fragment's spread
			}
		case len(stack) == 0:
			return fmt.Errorf("%q outside a selection set in %s", tok, q)
		default:
			field := tok
			if i+2 < len(toks) && toks[i+1] == ":" {
				field = toks[i+2]
				i += 2
			}
			if a, ok := abstractTypes[cur]; ok && field != "__typename" && !slices.Contains(a.fields, field) {
				return fmt.Errorf("field %q doesn't exist on type %q in %s", field, cur, q)
			}
			next = ""
			if cur != "" {
				next = fieldTypes[cur+"."+field]
			}
		}
	}
	if len(stack) != 0 {
		return fmt.Errorf("unclosed selection set in %s", q)
	}
	return nil
}

// fragmentOn reports an inline fragment on t GitHub rejects inside a
// selection on cur: types that share no object type.
func fragmentOn(cur, t string) error {
	if cur == "" || cur == t {
		return nil
	}
	possible := func(name string) []string {
		if a, ok := abstractTypes[name]; ok {
			return a.possible
		}
		return []string{name}
	}
	for _, p := range possible(t) {
		if slices.Contains(possible(cur), p) {
			return nil
		}
	}
	return fmt.Errorf("fragment on %q can never apply to %q", t, cur)
}

// checkedQuery fails t on a query GitHub's schema rejects, as GitHub fails
// the whole request.
func checkedQuery(t *testing.T, q string) {
	t.Helper()
	if err := checkSchema(q); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSchemaRejectsWhatGitHubRejects(t *testing.T) {
	for name, q := range map[string]string{
		// The query of https://github.com/giantswarm/beekeeper/pull/1990.
		"login on ProjectV2Owner": `query{repository(owner:"o",name:"r"){issue(number:1){projectItems(first:50){nodes{id project{id number owner{...on ProjectV2Owner{login}}}}}}}}`,
		"login on owner directly": `query{repository(owner:"o",name:"r"){issue(number:1){projectItems(first:50){nodes{project{owner{login}}}}}}}`,
		"field on a union":        `query{repository(owner:"o",name:"r"){issueOrPullRequest(number:1){state}}}`,
		"field on a union value":  `mutation{addProjectV2ItemById(input:{projectId:"p",contentId:"c"}){item{status:fieldValueByName(name:"Status"){name}}}}`,
		"fragment never applying": `query{repository(owner:"o",name:"r"){issueOrPullRequest(number:1){...on User{login}}}}`,
		"projectV2 on owner":      `query{repositoryOwner(login:"o"){projectV2(number:1){id}}}`,
	} {
		if checkSchema(q) == nil {
			t.Errorf("%s: %s passes", name, q)
		}
	}
}

func TestCheckSchemaPassesTheBoardsQueries(t *testing.T) {
	for name, q := range map[string]string{
		"an item":      `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){id state projectItems(first:50){nodes{` + itemFields + `}}}}}`,
		"an added one": `mutation($p:ID!,$c:ID!){addProjectV2ItemById(input:{projectId:$p,contentId:$c}){item{` + itemFields + `}}}`,
		"the board": `query($o:String!,$n:Int!,$q:String!,$c:String){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:$n){
items(first:80,after:$c,query:$q){pageInfo{hasNextPage endCursor} nodes{` + boardFields + `
content{...I}}}}}}}` + issueFields,
		"a state": `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issueOrPullRequest(number:$n){__typename ...on Issue{state} ...on PullRequest{state}}}}`,
	} {
		if err := checkSchema(q); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
