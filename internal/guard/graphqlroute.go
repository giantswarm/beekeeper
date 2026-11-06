package guard

import (
	"regexp"
	"slices"
	"strings"
)

const ghRepoEnv = "GH_REPO"

var (
	// graphqlRepository: a repository field of a GraphQL query, its owner and
	// name in either order, each a literal or a variable.
	graphqlRepository = regexp.MustCompile(`\brepository\s*\(\s*(owner|name)\s*:\s*("[\w.-]+"|\$\w+)\s*,?\s*(owner|name)\s*:\s*("[\w.-]+"|\$\w+)`)
	graphqlMutation   = regexp.MustCompile(`\bmutation\b`)
)

// graphqlRoute returns cmd with GH_REPO naming the repository of each plain
// `gh api graphql` read that asks about one owner's repositories. gh api
// graphql names no repository of its own, so an agent's gh takes the App's
// identity, and a personal account's repository, which REST reads with the
// person's login, answers NOT_FOUND. GH_REPO gives GraphQL the identity REST
// gets. A mutation stays as written, and so does a call that sets GH_REPO.
func graphqlRoute(cmd, cwd string) string {
	if strings.Contains(cmd, ghRepoEnv+"=") {
		return cmd
	}
	sc := scanShell(cmd)
	segs := sc.segments()
	for _, sg := range slices.Backward(segs) {
		words := segWords(sc, sg)
		if len(words) < 2 || words[0] != "gh" || ghAPIEndpoint(words) != executionGraphQL {
			continue
		}
		repo := graphqlRepo(words, cwd)
		if repo == "" {
			continue
		}
		at := sg.start + len(sc.plain[sg.start:sg.end]) - len(strings.TrimLeft(sc.plain[sg.start:sg.end], " \t\n"))
		cmd = cmd[:at] + ghRepoEnv + "=" + repo + " " + cmd[at:]
	}
	return cmd
}

// graphqlRepo is the repository a read's query names, "" for a mutation, for
// none, or for repositories of several owners, which no one identity reads.
func graphqlRepo(words []string, cwd string) string {
	body := strings.Join(words, "\n") + "\n" + ghAPIBodies(words, cwd)
	if graphqlMutation.MatchString(body) {
		return ""
	}
	var repo, owner string
	for _, m := range graphqlRepository.FindAllStringSubmatch(body, -1) {
		if m[1] == m[3] {
			return ""
		}
		o, n := graphqlValue(m[2], words), graphqlValue(m[4], words)
		if m[1] == "name" {
			o, n = n, o
		}
		if o == "" || n == "" || owner != "" && !strings.EqualFold(o, owner) {
			return ""
		}
		if owner == "" {
			repo, owner = o+"/"+n, o
		}
	}
	if !shellSafe.MatchString(repo) {
		return ""
	}
	return repo
}

// graphqlValue is a query's literal, or the value of its variable as the
// call's -f or -F word gives it; "" for one it does not.
func graphqlValue(v string, words []string) string {
	if name, ok := strings.CutPrefix(v, "$"); ok {
		for i, w := range words {
			for _, flag := range []string{"-f", "-F", flagRawField, flagField} {
				val, ok := strings.CutPrefix(w, flag+"="+name+"=")
				if !ok {
					val, ok = strings.CutPrefix(w, flag+name+"=")
				}
				if !ok && w == flag && i+1 < len(words) {
					val, ok = strings.CutPrefix(words[i+1], name+"=")
				}
				if ok && shellSafe.MatchString(val) {
					return val
				}
			}
		}
		return ""
	}
	return strings.Trim(v, `"`)
}
