package github

import (
	"regexp"
	"strings"
)

// graphQLWrite is an operation that is no read. It may stand anywhere in a
// document, so any occurrence counts, a field of that name included.
var graphQLWrite = regexp.MustCompile(`(?i)\b(mutation|subscription)\b`)

// GraphQLRead says args, those after gh api, are a GraphQL read that can be
// judged in full: the endpoint, one literal query that holds no write, literal
// variables, and only the flags that shape the output. Anything else, a file or
// an empty word (a guard's dynamic one) above all, is no read here.
func GraphQLRead(args []string) bool {
	endpoint, queries := "", 0
	field := func(value string) bool {
		key, val, ok := strings.Cut(value, "=")
		switch {
		case !ok || key == "" || val == "" || strings.HasPrefix(val, "@"):
			return false
		case key != "query":
			return true
		}
		queries++
		return !graphQLWrite.MatchString(val)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "":
			return false
		case a == "-f" || a == "-F" || a == ghFlagField || a == ghFlagRawField:
			i++
			if i == len(args) || !field(args[i]) {
				return false
			}
		case strings.HasPrefix(a, "--field=") || strings.HasPrefix(a, "--raw-field="):
			if _, value, _ := strings.Cut(a, "="); !field(value) {
				return false
			}
		case a == ghFlagJQ || a == "-q" || a == ghFlagTemplate || a == "-t" || a == "--cache":
			i++
			if i == len(args) || args[i] == "" {
				return false
			}
		case a == "--paginate" || a == "--slurp" || a == "--silent" || a == "--include" || a == "-i" || a == "--verbose":
		case strings.HasPrefix(a, "--jq=") || strings.HasPrefix(a, "--template=") || strings.HasPrefix(a, "--cache="):
		case strings.HasPrefix(a, "-") || endpoint != "":
			return false
		default:
			endpoint = a
		}
	}
	return endpoint == "graphql" && queries == 1
}
