package guard

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

var (
	// repoQualifier: a search query's repo: qualifier, raw or URL-encoded; an
	// excluding -repo: narrows nothing and is no match.
	repoQualifier = regexp.MustCompile(`(?i)(?:^|[\s(+])repo(?::|%3A)`)
	// searchIssuesPath: the REST search over issues and pull requests.
	searchIssuesPath = regexp.MustCompile(`^(?:https?://api\.github\.com)?/?search/issues(?:[?#].*)?$`)
	// ghSearchValued are gh search issues' and prs' flags that take the next
	// word as their value.
	ghSearchValued = map[string]bool{"--app": true, "--assignee": true, "--author": true, "--closed": true, "--commenter": true,
		"--comments": true, "--created": true, "-L": true, "--limit": true, "--interactions": true, "--involves": true,
		"-l": true, "--label": true, "--language": true, "--match": true, "--mentions": true, "--milestone": true,
		"--owner": true, "--reactions": true, "--sort": true, "--order": true, "--state": true, "--team-mentions": true,
		"--updated": true, "--visibility": true, "--json": true, "-q": true, "--jq": true, "-t": true, flagTemplate: true,
		"-B": true, "--base": true, "-H": true, "--head": true, "--checks": true, "--merged-at": true, "--review": true,
		"--review-requested": true, "--reviewed-by": true}
)

// searchRefusal says why cmd is refused in a session beekeeper started: a
// search over issues or pull requests that spans more than one repository
// (gh search issues or prs, or gh api search/issues), which GitHub answers
// from the repositories the App's token an agent's gh carries can read and
// drops every other one without a word, so a scan reads "no match" for a
// repository it never saw. A search scoped to one repository fails loudly on
// one it cannot read and passes. A person's own session passes every call;
// the start is read only for a matching one.
func (h Hook) searchRefusal(cmd, session string) string {
	sc := scanShell(cmd)
	for _, sg := range sc.segments() {
		what, repos := ghSearch(shellWords(sc.plain[sg.start:sg.end]))
		if what == "" || repos == 1 {
			continue
		}
		if session == "" || h.Started == nil || !h.Started(session) {
			return ""
		}
		return "Refused: " + what + " spans more than one repository, and GitHub answers it from the repositories the App's " +
			"token an agent's `gh` carries can read: a private repository it cannot read drops out of the results without " +
			"an error, so its \"no match\" is no answer. List each repository instead, which fails loudly on one it cannot " +
			"read: `gh issue list --repo <owner/repo> --state all --search \"<query>\"` (`gh pr list` for pull requests), " +
			"the repositories from `gh repo list <owner> --no-archived --json nameWithOwner`. A search with one `--repo` " +
			"(or one `repo:` qualifier) passes. Report a repository the listing cannot read as a problem, never as no match."
	}
	return ""
}

// ghSearch names the issue search the words run and the repositories its
// query is scoped to (--repo values and repo: qualifiers), "" when they run
// none.
func ghSearch(words []string) (string, int) {
	k := commandAt(words)
	if k+2 >= len(words) || path.Base(words[k]) != "gh" {
		return "", 0
	}
	switch words[k+1] {
	case "search":
		if sub := words[k+2]; sub == "issues" || sub == "prs" {
			return "`gh search " + sub + "`", searchRepos(words[k+3:])
		}
	case ghAPISub:
		ep := ghAPIEndpoint(words)
		if !searchIssuesPath.MatchString(ep) {
			return "", 0
		}
		n := 0
		if _, q, ok := strings.Cut(ep, "?"); ok {
			if v, err := url.ParseQuery(q); err == nil {
				n += len(repoQualifier.FindAllString(v.Get("q"), -1))
			}
		}
		for i, w := range words {
			if (w == "-f" || w == "-F" || w == "--raw-field" || w == "--field") && i+1 < len(words) {
				if q, ok := strings.CutPrefix(words[i+1], "q="); ok {
					n += len(repoQualifier.FindAllString(q, -1))
				}
			}
		}
		return "`gh api search/issues`", n
	}
	return "", 0
}

// searchRepos counts the repositories gh search's arguments scope it to.
func searchRepos(args []string) int {
	n := 0
	for i := 0; i < len(args); i++ {
		switch w := args[i]; {
		case w == "-R" || w == "--repo":
			n++
			i++
		case strings.HasPrefix(w, "--repo=") || strings.HasPrefix(w, "-R="):
			n++
		case ghSearchValued[w]:
			i++
		case strings.HasPrefix(w, "-"):
		default:
			n += len(repoQualifier.FindAllString(w, -1))
		}
	}
	return n
}
