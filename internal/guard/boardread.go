package guard

import (
	"regexp"
	"strings"
)

var (
	// itemList: gh project item-list at a command position, gh by name or
	// path, up to the end of its command.
	itemList = regexp.MustCompile(`(?m)` + pos + `(?:[^\s;&|()'"]*/)?gh\s+project\s+item-list\b([^;&|\n]*)`)
	// itemQuery: item-list's --query, which narrows the read to the matches.
	itemQuery = regexp.MustCompile(`(?:^|\s)--query(?:=|\s)`)
	// graphQL: gh api graphql at a command position; the query is read from
	// there to the end of the text, since it may span lines.
	graphQL = regexp.MustCompile(`(?m)` + pos + `(?:[^\s;&|()'"]*/)?gh\s+api\s+graphql\b`)
	// projectItems: a projectV2 items connection and its arguments; an
	// issue's projectItems is no match (no word boundary before items).
	projectItems = regexp.MustCompile(`\bitems\s*\(([^)]*)\)`)
	itemsQuery   = regexp.MustCompile(`\bquery\s*:`)
)

// boardReadRefusal is boardReadRefusal with the hook's GraphQL reading.
func (h Hook) boardReadRefusal(cmd string) string {
	return boardReadRefusal(cmd, func() string {
		if h.GraphQL == nil {
			return ""
		}
		return h.GraphQL()
	})
}

// boardReadRefusal says why cmd is refused when it reads a project board's
// items unfiltered: gh project item-list without --query, or a gh api
// graphql query over projectV2 items without a narrowing query: argument.
// Such a read pages the whole board with its field values and spends the
// GraphQL limit every session on the machine shares. budget is the GraphQL
// budget as beekeeper last read it, "" when unknown; read only for a refusal.
func boardReadRefusal(cmd string, budget func() string) string {
	what := ""
	for _, m := range itemList.FindAllStringSubmatch(cmd, -1) {
		if !itemQuery.MatchString(m[1]) {
			what = "`gh project item-list` without `--query`"
			break
		}
	}
	if what == "" {
		if m := graphQL.FindStringIndex(cmd); m != nil {
			q := cmd[m[1]:]
			if strings.Contains(q, "projectV2") {
				for _, a := range projectItems.FindAllStringSubmatch(q, -1) {
					if !itemsQuery.MatchString(a[1]) {
						what = "a `gh api graphql` query over a project's `items` without a narrowing `query:`"
						break
					}
				}
			}
		}
	}
	if what == "" {
		return ""
	}
	left := budget()
	if left == "" {
		left = "unknown (`beekeeper budget` reads it)"
	}
	return "Refused: " + what + " pages the whole board with every item's field values, one request per 100 items " +
		"at an estimated 10 to 100 GraphQL points each: a board of thousands of items spends the hourly limit every session " +
		"on this machine shares. GraphQL left: " + left + ".\n" +
		"Instead:\n" +
		"- add an issue or PR: `gh project item-add <board> --owner <org> --url <issue-url>` (its JSON names the item id)\n" +
		"- set a field: `gh project item-edit --id <item-id> --project-id <project-id> --field-id <field-id> ...` with the item id from item-add, or from the issue's own `projectItems` (`gh api graphql` over `repository.issue.projectItems`)\n" +
		"- move an item's Status: `beekeeper board move <owner/repo#n> <status>`\n" +
		"- find items: `gh project item-list <board> --owner <org> --query \"<filter>\"`, or `projectV2.items(query: \"<filter>\")`"
}
