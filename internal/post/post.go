// Package post checks a report before it is posted with the Slack
// connector, whose message is standard Markdown it converts for Slack: that
// every pull request or issue it mentions is a link to that pull request or
// issue in its own repository, and that its times are the person's.
package post

import (
	"fmt"
	"regexp"
	"slices"
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
	ref = regexp.MustCompile(`(^|[^\w&/.#-])((?:[\w.-]+/)?[\w.-]*#(\d+))\b`)
	utc = regexp.MustCompile(`\b(?:UTC|GMT)\b`)
	// shortRef is an issue or pull request named as #N or owner/repo#N;
	// "note #N", "timer #N" and "--on <ref>" are not issue references.
	shortRef = regexp.MustCompile(`(?i)(?:^|[^\w&/#])((?:note|timer|--on)\s+['"]?)?((?:([\w.-]+/[\w.-]+))?#(\d+))\b`)
	// nonIssueRef is "note #N", "timer #N" or "--on <ref>", which may span a line break.
	nonIssueRef = regexp.MustCompile(`(?i)\b(?:note|timer|--on)\s+['"]?(?:[\w.-]+/[\w.-]+)?#\d+\b`)
	// refURL is the full URL of a GitHub issue or pull request.
	refURL = regexp.MustCompile(`(?i)https?://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)`)
	// anyURL is an http or https URL, whose #fragment is an anchor, never a bare #N.
	anyURL = regexp.MustCompile(`(?i)https?://[^\s)>\]"']+`)
	// cssPropPrefix is the part of a line before a #colour in a CSS declaration.
	cssPropPrefix = regexp.MustCompile(`(?i)(?:--[\w-]+|\b(?:(?:[\w-]+-)?(?:color|background|border|outline|fill|stroke|shadow|rule|caret|accent|tint|shade|surface|theme|flood|lighting|stop|decoration|column-rule)|(?:border|outline)-(?:top|right|bottom|left)(?:-[\w-]+)?|background-image|list-style))\s*:[^;\n{}]*$`)
	// cssFuncPrefix is the part of a line before a #colour in a CSS colour function.
	cssFuncPrefix = regexp.MustCompile(`(?i)\b(?:(?:(?:repeating-)?(?:linear|radial|conic)-)?gradient|color-mix|light-dark|var|drop-shadow)\([^)\n]*$`)
	// cssHexCandidate is a bare numeric #rgb, #rgba, #rrggbb or #rrggbbaa token.
	cssHexCandidate = regexp.MustCompile(`(?:^|[^\w&/.#-])(#(?:\d{8}|\d{6}|\d{4}|\d{3}))\b`)
)

// Check returns what is wrong with a report's links, one problem per
// entry; none: it passes.
func Check(text string) []string {
	if strings.TrimSpace(text) == "" {
		return []string{"the message is empty"}
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	plain := stripCode(text)
	for _, m := range slackLink.FindAllString(plain, -1) {
		add("%s is Slack's link syntax: the connector takes Markdown, write [label](url)", m)
	}
	for _, m := range link.FindAllStringSubmatch(plain, -1) {
		checkLink(m[0], m[1], m[2], add)
	}
	outside := blankCSSColors(link.ReplaceAllString(plain, " "))
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
	if abbr != "UTC" && abbr != "GMT" && utc.MatchString(stripCode(text)) {
		problems = append(problems, fmt.Sprintf("a time is in UTC: every time in the report is in %s (%s)", zone, abbr))
	}
	return problems
}

// Report is every check a report passes: Check and Zone.
func Report(text string, zone *time.Location, now time.Time) []string {
	return append(Check(text), Zone(text, zone, now)...)
}

// note is a beekeeper note or timer named with a #: "note #84".
var note = regexp.MustCompile(`(?i)\b(note|timer)\s+#(\d+)`)

// Linked rewrites text so it passes Check: every [owner/]repo#n or bare #n
// outside a link and a code span becomes a link to its issue, its owner the
// one owners has for the repository, a bare #n in here (owner/repo); a
// beekeeper note or timer is "note <n>"; a reference it cannot place loses
// its # ("repo 12").
func Linked(text string, owners map[string]string, here string) string {
	text = note.ReplaceAllString(text, "$1 $2")
	var b strings.Builder
	last := 0
	for _, span := range spans(text) {
		b.WriteString(linkRefs(text[last:span[0]], owners, here))
		b.WriteString(text[span[0]:span[1]])
		last = span[1]
	}
	b.WriteString(linkRefs(text[last:], owners, here))
	return b.String()
}

// spans are the links, code spans and CSS colour literals of text, in order,
// not overlapping.
func spans(text string) [][2]int {
	out := codeRanges(text)
	for _, m := range link.FindAllStringIndex(text, -1) {
		if !slices.ContainsFunc(out, func(s [2]int) bool { return m[0] < s[1] && s[0] < m[1] }) {
			out = append(out, [2]int{m[0], m[1]})
		}
	}
	for _, m := range cssColorRanges(text) {
		if !slices.ContainsFunc(out, func(s [2]int) bool { return m[0] < s[1] && s[0] < m[1] }) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b [2]int) int { return a[0] - b[0] })
	return out
}

// linkRefs links the references of plain text.
func linkRefs(text string, owners map[string]string, here string) string {
	return ref.ReplaceAllStringFunc(text, func(m string) string {
		sub := ref.FindStringSubmatch(m)
		lead, whole, n := sub[1], sub[2], sub[3]
		repo, _, _ := strings.Cut(whole, "#")
		full := ""
		switch owner, name, hasOwner := strings.Cut(repo, "/"); {
		case repo == "":
			full = here
		case hasOwner && owner != "" && name != "":
			full = repo
		case !hasOwner && owners[strings.ToLower(repo)] != "":
			full = owners[strings.ToLower(repo)] + "/" + repo
		}
		if full == "" {
			if strings.HasSuffix(repo, "/") {
				return lead + repo + n
			}
			return lead + strings.TrimSpace(repo+" "+n)
		}
		_, name, _ := strings.Cut(full, "/")
		return fmt.Sprintf("%s[%s#%s](https://github.com/%s/issues/%s)", lead, name, n, full, n)
	})
}

// Owners maps each repository name of the owner/repo names to its owner,
// lowercased, for Linked.
func Owners(repos ...string) map[string]string {
	out := map[string]string{}
	for _, r := range repos {
		if o, name, ok := strings.Cut(r, "/"); ok && o != "" && name != "" {
			out[strings.ToLower(name)] = o
		}
	}
	return out
}

// Unlinked lists the bare #N and owner/repo#N references in text (outside
// fenced code blocks, inline code spans, CSS colour literals and URLs) that
// have no matching full GitHub issue or pull request URL in text, each
// formatted as "<ref> without its full URL (<url>)". When here is "owner/repo"
// (or text links one repository), a bare #N names that repository in its URL.
func Unlinked(text, here string) []string {
	var seen, out []string
	for _, line := range UnlinkedLines(text, here) {
		for _, item := range line {
			ref, _, _ := strings.Cut(item, " ")
			if !slices.Contains(seen, ref) {
				seen = append(seen, ref)
				out = append(out, item)
			}
		}
	}
	return out
}

// UnlinkedLines returns, for each line of text (0-indexed), the unlinked #N
// and owner/repo#N references on that line, evaluated against the full
// document's code boundaries and linked URLs.
func UnlinkedLines(text, here string) [][]string {
	plain := stripCode(text)
	urls := refURL.FindAllStringSubmatch(plain, -1)
	if here == "" {
		here = singleRepo(urls)
	}
	cleaned := blankCSSColors(blankPreservingNewlines(blankPreservingNewlines(plain, anyURL), nonIssueRef))
	lines := strings.Split(cleaned, "\n")
	out := make([][]string, len(lines))
	for i, line := range lines {
		var seen []string
		for _, m := range shortRef.FindAllStringSubmatch(line, -1) {
			if m[1] != "" {
				continue
			}
			ref, repo, num := m[2], m[3], m[4]
			linked := slices.ContainsFunc(urls, func(u []string) bool {
				return u[2] == num && (repo == "" || strings.EqualFold(u[1], repo))
			})
			if linked || slices.Contains(seen, ref) {
				continue
			}
			seen = append(seen, ref)
			r := repo
			if r == "" {
				r = here
			}
			if r == "" {
				r = "<owner>/<repo>"
			}
			out[i] = append(out[i], fmt.Sprintf("%s without its full URL (https://github.com/%s/issues/%s)", ref, r, num))
		}
	}
	return out
}

// SingleRepo returns the single owner/repo that text names by a full GitHub
// issue or pull request URL or an owner/repo#N reference, or "" when none or
// more than one.
func SingleRepo(text string) string {
	plain := stripCode(text)
	if r := singleRepo(refURL.FindAllStringSubmatch(plain, -1)); r != "" {
		return r
	}
	var repo string
	cleaned := blankCSSColors(blankPreservingNewlines(blankPreservingNewlines(plain, anyURL), nonIssueRef))
	for _, m := range shortRef.FindAllStringSubmatch(cleaned, -1) {
		if m[1] != "" || m[3] == "" {
			continue
		}
		if repo == "" {
			repo = m[3]
		} else if !strings.EqualFold(repo, m[3]) {
			return ""
		}
	}
	return repo
}

func singleRepo(urls [][]string) string {
	var repo string
	for _, u := range urls {
		if repo == "" {
			repo = u[1]
		} else if !strings.EqualFold(repo, u[1]) {
			return ""
		}
	}
	return repo
}

// stripCode blanks Markdown fenced code blocks and inline code spans in text,
// keeping line breaks in place so line numbers do not shift.
func stripCode(text string) string {
	ranges := codeRanges(text)
	if len(ranges) == 0 {
		return text
	}
	b := []byte(text)
	for _, r := range ranges {
		for i := r[0]; i < r[1]; i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// codeRanges returns the byte ranges of Markdown fenced code blocks and inline
// code spans in text. Fenced blocks open at the start of a line with at least
// three backticks or tildes and close only on a line whose leading fence of the
// same character is at least as long and has no trailing non-blank text (or at
// EOF when unclosed). Inline code spans open with a backtick run and close at
// the next backtick run of the exact same length before the next fenced block.
func codeRanges(text string) [][2]int {
	var out [][2]int
	for i := 0; i < len(text); {
		if i == 0 || text[i-1] == '\n' {
			if ch, fenceLen, afterLine, ok := opensFenceAt(text, i); ok {
				end, next := closeFenceAt(text, afterLine, ch, fenceLen)
				out = append(out, [2]int{i, end})
				i = next
				continue
			}
		}
		if text[i] != '`' {
			i++
			continue
		}
		k := i
		for k < len(text) && text[k] == '`' {
			k++
		}
		if end, ok := closeInlineSpan(text, k, k-i); ok {
			out = append(out, [2]int{i, end})
			i = end
			continue
		}
		i = k
	}
	return out
}

func opensFenceAt(text string, lineStart int) (ch byte, fenceLen, afterLine int, ok bool) {
	j := lineStart
	for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
		j++
	}
	if j >= len(text) || (text[j] != '`' && text[j] != '~') {
		return 0, 0, 0, false
	}
	ch = text[j]
	k := j
	for k < len(text) && text[k] == ch {
		k++
	}
	if k-j < 3 {
		return 0, 0, 0, false
	}
	info := text[k:]
	afterLine = len(text)
	if nl := strings.IndexByte(text[k:], '\n'); nl >= 0 {
		info = text[k : k+nl]
		afterLine = k + nl + 1
	}
	if ch == '`' && strings.ContainsRune(info, '`') {
		return 0, 0, 0, false
	}
	return ch, k - j, afterLine, true
}

func closeFenceAt(text string, pos int, ch byte, minLen int) (end, next int) {
	for pos < len(text) {
		line := text[pos:]
		lineEnd := len(text)
		nextPos := len(text)
		if nl := strings.IndexByte(text[pos:], '\n'); nl >= 0 {
			line = text[pos : pos+nl]
			lineEnd = pos + nl
			nextPos = pos + nl + 1
		}
		if isClosingFence(line, ch, minLen) {
			return lineEnd, nextPos
		}
		pos = nextPos
	}
	return len(text), len(text)
}

func isClosingFence(line string, ch byte, minLen int) bool {
	trimmed := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	return n >= minLen && strings.TrimRight(trimmed[n:], " \t\r") == ""
}

func closeInlineSpan(text string, start, spanLen int) (int, bool) {
	for m := start; m < len(text); {
		if m > 0 && text[m-1] == '\n' {
			if _, _, _, ok := opensFenceAt(text, m); ok {
				return 0, false
			}
		}
		if text[m] != '`' {
			m++
			continue
		}
		end := m
		for end < len(text) && text[end] == '`' {
			end++
		}
		if end-m == spanLen {
			return end, true
		}
		m = end
	}
	return 0, false
}

func blankPreservingNewlines(s string, re *regexp.Regexp) string {
	matches := re.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	b := []byte(s)
	for _, m := range matches {
		for i := m[0]; i < m[1]; i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

func cssColorRanges(s string) [][2]int {
	var out [][2]int
	offset := 0
	for line := range strings.SplitSeq(s, "\n") {
		for _, m := range cssHexCandidate.FindAllStringSubmatchIndex(line, -1) {
			before := line[:m[2]]
			if cssPropPrefix.MatchString(before) || cssFuncPrefix.MatchString(before) {
				out = append(out, [2]int{offset + m[2], offset + m[3]})
			}
		}
		offset += len(line) + 1
	}
	return out
}

func blankCSSColors(s string) string {
	ranges := cssColorRanges(s)
	if len(ranges) == 0 {
		return s
	}
	b := []byte(s)
	for _, r := range ranges {
		for i := r[0]; i < r[1]; i++ {
			b[i] = ' '
		}
	}
	return string(b)
}
