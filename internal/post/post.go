// Package post checks a report before it is posted with the Slack
// connector, whose message is standard Markdown it converts for Slack: that
// every pull request or issue it mentions is a link to that pull request or
// issue in its own repository, and that its times are the person's.
package post

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	// link is a Markdown link [label](url).
	link = regexp.MustCompile(`\[([^\]\n]*)\]\(([^)\s]+)\)`)
	// slackLink is Slack's own <url|label>, which the connector does not
	// take: it expects Markdown.
	slackLink = regexp.MustCompile(`<(?:https?://|mailto:)[^>\s]*>`)
	// github is a pull request or issue URL.
	github = regexp.MustCompile(`^https://github\.com/([\w.-]+)/([\w.-]+)/(pull|issues)/(\d+)(?:[/?#]\S*)?$`)
	// ref is [owner/]repo#n or a bare #n. The character before it is
	// matched to exclude HTML entities (&#123;) and words glued to it.
	ref      = regexp.MustCompile(`(^|[^\w&/.#-])((?:[\w.-]+/)?[\w.-]*#(\d+))\b`)
	codeSpan = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")
	utc      = regexp.MustCompile(`\b(?:UTC|GMT)\b`)
)

// Check returns what is wrong with a report's links, one problem per
// entry; none: it passes.
func Check(text string) []string {
	if strings.TrimSpace(text) == "" {
		return []string{"the message is empty"}
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	plain := codeSpan.ReplaceAllString(text, " ")
	for _, m := range slackLink.FindAllString(plain, -1) {
		add("%s is Slack's link syntax: the connector takes Markdown, write [label](url)", m)
	}
	for _, m := range link.FindAllStringSubmatch(plain, -1) {
		checkLink(m[0], m[1], m[2], add)
	}
	outside := link.ReplaceAllString(plain, " ")
	for _, l := range strings.Split(outside, "\n") {
		for _, m := range ref.FindAllStringSubmatchIndex(l, -1) {
			n := l[m[6]:m[7]]
			add("%s is not a link: write [<repo>#%s](https://github.com/<owner>/<repo>/pull/%s) (issues/%s for an issue); "+
				"a beekeeper note or timer is \"note %s\", without the #", l[m[4]:m[5]], n, n, n, n)
		}
	}
	return problems
}

// checkLink checks one Markdown link: a pull request's or issue's carries
// a label naming the same repository and number, and a label naming one
// links it.
func checkLink(whole, label, url string, add func(string, ...any)) {
	g := github.FindStringSubmatch(url)
	refs := ref.FindAllStringSubmatch(" "+label, -1)
	switch {
	case g == nil && len(refs) > 0:
		add("%s names %s but links no GitHub pull request or issue", whole, refs[0][2])
	case g == nil:
	case len(refs) == 0:
		add("%s needs the label %s#%s", whole, g[2], g[4])
	default:
		owner, repo, n := g[1], g[2], g[4]
		for _, r := range refs {
			name, num, _ := strings.Cut(r[2], "#")
			o, rp, hasOwner := strings.Cut(name, "/")
			if !hasOwner {
				o, rp = "", name
			}
			if !strings.EqualFold(rp, repo) || hasOwner && !strings.EqualFold(o, owner) || num != n {
				add("%s is labelled %s but links %s/%s#%s", whole, r[2], owner, repo, n)
			}
		}
	}
}

// Zone checks that a report tells its times in zone, the person's: its
// first line names the zone's abbreviation at now (EEST, CET), and no time
// is in UTC unless zone is.
func Zone(text string, zone *time.Location, now time.Time) []string {
	at := now.In(zone)
	abbr := at.Format("MST")
	var problems []string
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if !strings.Contains(first, abbr) {
		problems = append(problems, fmt.Sprintf("the first line names no %s: give its time range in %s, e.g. %s–%s %s",
			abbr, zone, at.Add(-time.Hour).Format("15:04"), at.Format("15:04"), abbr))
	}
	if abbr != "UTC" && abbr != "GMT" && utc.MatchString(codeSpan.ReplaceAllString(text, " ")) {
		problems = append(problems, fmt.Sprintf("a time is in UTC: every time in the report is in %s (%s)", zone, abbr))
	}
	return problems
}

// Report is every check a report passes: Check and Zone.
func Report(text string, zone *time.Location, now time.Time) []string {
	return append(Check(text), Zone(text, zone, now)...)
}
