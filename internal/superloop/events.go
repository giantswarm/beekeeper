package superloop

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// issueURL is an issue's URL: a problem naming one is filed.
var issueURL = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/issues/\d+`)

// pullURL is a pull request's URL.
var pullURL = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// filedReportClause matches a clause in a report that names newly filed issues,
// keeping dots inside URLs and version tokens while stopping at sentence ends.
var filedReportClause = regexp.MustCompile(`(?i)\b(?:filed|created|opened|follow[- ]?ups?|problems?\s+found)\b(?:[^.;\n]|\.[^.;\s])*`)

// filedIssueURLs returns the issue URLs a report names as newly filed or
// found problems, leaving out the closed task issue the report summarizes.
func filedIssueURLs(report string) []string {
	var out []string
	for _, clause := range filedReportClause.FindAllString(report, -1) {
		for _, u := range issueURL.FindAllString(clause, -1) {
			if !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
	}
	return out
}

// issueRefFromURL converts https://github.com/owner/repo/issues/n (or /pull/n)
// to owner/repo#n.
func issueRefFromURL(u string) string {
	s := strings.TrimPrefix(u, "https://github.com/")
	for _, sep := range []string{"/issues/", "/pull/"} {
		if repo, n, ok := strings.Cut(s, sep); ok && repo != "" && n != "" {
			return repo + "#" + n
		}
	}
	return ""
}

// Filed reports whether a problem found names its issue's URL: it is
// filed.
func Filed(problem string) bool { return issueURL.MatchString(problem) }

// problemText is an agents.problem's finding, without its agent's name.
func problemText(e Event) string { return strings.TrimPrefix(e.Detail, e.Agent+": ") }

// matches reports whether a problem says what a review's body says: most
// of its words (four letters or more) are the body's.
func matches(problem, body string) bool {
	in := map[string]bool{}
	for _, w := range words(body) {
		in[w] = true
	}
	ws := words(problem)
	if len(ws) == 0 {
		return false
	}
	n := 0
	for _, w := range ws {
		if in[w] {
			n++
		}
	}
	return n*10 >= len(ws)*6
}

// words are s's words of four letters or more, lower case.
func words(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) {
		if len(w) >= 4 {
			out = append(out, w)
		}
	}
	return out
}

// noteRef is a note a park's wait names (#12), not an issue's owner/repo#n.
var noteRef = regexp.MustCompile(`(?:^|[^\w/.-])#\d+\b`)

// pairWithin is how far apart a report and its done may be logged.
const pairWithin = 5 * time.Second

// EventAgent is the roster name of the agent an event of verb concerns:
// the name its detail opens with ("<name> done: …", "<name>: …"), which
// agents idle and agents park write from the roster, else by, the name
// of the party that logged it (its session's title, which may differ).
func EventAgent(verb, detail, by string) string {
	sep := ": "
	if verb == VerbDone || verb == VerbIdle {
		sep = " done: "
	}
	if strings.HasPrefix(verb, "agents.") {
		if name, _, ok := strings.Cut(detail, sep); ok && name != "" {
			return name
		}
	}
	return by
}

// ParkedOn reads an agents.park detail ("<name>: parked on <on>: <waits>"
// or "<name>: parked: <waits>"): what it is parked on and what it waits
// for.
func ParkedOn(detail string) (on, waits string) {
	_, rest, ok := strings.Cut(detail, ": parked")
	if !ok {
		return "", ""
	}
	if r, isOn := strings.CutPrefix(rest, " on "); isOn {
		on, waits, _ = strings.Cut(r, ": ")
		return on, waits
	}
	return "", strings.TrimPrefix(rest, ": ")
}
