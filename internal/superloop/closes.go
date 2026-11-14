package superloop

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	absorbLead        = regexp.MustCompile(`(?i)\b(?:combin(?:e|es|ed|ing)|supersed(?:e|es|ed|ing)|absorb(?:s|ed|ing)?|fold(?:s|ed|ing)?\s+(?:in|from)|original\s+(?:pr|pull\s+request)s?)\b`)
	absorbHeader      = regexp.MustCompile(`(?i)^(?:#+\s*|\*\*\s*)?(?:combin(?:e|es|ed|ing)|supersed(?:e|es|ed|ing)|absorb(?:s|ed|ing)?|fold(?:s|ed|ing)?\s+(?:in|from)|original)(?:\s+(?:original|pr|prs|pull\s+requests?))*\s*:?\s*(?:\*\*)?$`)
	absorbFirstGap    = regexp.MustCompile(`(?i)^[\s:]*(?:(?:from|with|in|original|originals|pr|prs|pull\s+requests?)\b[\s:]*)*$`)
	absorbNextGap     = regexp.MustCompile(`(?i)^[\s,]*(?:(?:and|&|\+|pr|prs|pull\s+requests?)\b[\s,]*)*$`)
	absorbBulletLead  = regexp.MustCompile(`^(?:[-*+]|\d+\.)\s+`)
	commitPrefixLead  = regexp.MustCompile(`(?i)^[a-z]+(?:\([^)]*\))?!?:\s*`)
	affirmativeClause = regexp.MustCompile(`(?i)^[\s()*]*(?:(?:this(?:\s+(?:pr|pull\s+request|change|commit|batch|work))?|also|and)\s+[\s()*]*)*[\s()*]*$`)
	shortPullMention  = regexp.MustCompile(`(?:https://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+))|(?:https://github\.com/[\w.-]+/[\w.-]+/issues/\d+)|\b([\w.-]+/[\w.-]+)#(\d+)\b|(?:^|[^\w/.-])(?:pr\s*#?|pull\s+request\s*#?|#)(\d+)\b`)
	closingLead       = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b`)
	closingHeader     = regexp.MustCompile(`(?i)^(?:#+\s*|\*\*\s*)?(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)(?:\s+issues?)?\s*:?\s*(?:\*\*)?$`)
	closingFirstGap   = regexp.MustCompile(`(?i)^[\s:*]*(?:(?:(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?|issues?)\b[\s:*]*)*$`)
	closingNextGap    = regexp.MustCompile(`(?i)^[\s,]*(?:(?:and|&|\+|(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?|issues?)\b[\s,]*)*$`)
	shortIssueRef     = regexp.MustCompile(`(?:https://github\.com/([\w.-]+/[\w.-]+)/issues/(\d+))|(?:https://github\.com/[\w.-]+/[\w.-]+/pull/\d+)|\b([\w.-]+/[\w.-]+)#(\d+)\b|(?:^|[^\w/.-])(?:#|/)(\d+)\b`)
)

func leadingMarkdownFence(trimmed string) string {
	if len(trimmed) < 3 {
		return ""
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return ""
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return ""
	}
	if ch == '`' && strings.ContainsRune(trimmed[n:], '`') {
		return ""
	}
	return trimmed[:n]
}

func closesMarkdownFence(trimmed, marker string) bool {
	if marker == "" || len(trimmed) < len(marker) {
		return false
	}
	ch := marker[0]
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	return n >= len(marker) && strings.TrimSpace(trimmed[n:]) == ""
}

func stripInlineCodeSpans(line string) string {
	if !strings.ContainsRune(line, '`') {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		n := 0
		for i+n < len(line) && line[i+n] == '`' {
			n++
		}
		tickRun := line[i : i+n]
		closeIdx := -1
		for j := i + n; j+n <= len(line); j++ {
			if line[j:j+n] != tickRun {
				continue
			}
			if j > i+n && line[j-1] == '`' {
				continue
			}
			if j+n < len(line) && line[j+n] == '`' {
				continue
			}
			closeIdx = j
			break
		}
		if closeIdx < 0 {
			b.WriteString(tickRun)
			i += n
			continue
		}
		b.WriteString(" <code-span> ")
		i = closeIdx + n
	}
	return b.String()
}

func sanitizeMarkdownBody(body string) string {
	var out []string
	var paragraph []string
	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		joined := stripInlineCodeSpans(strings.Join(paragraph, "\n"))
		out = append(out, strings.Split(joined, "\n")...)
		paragraph = paragraph[:0]
	}
	fenceMarker := ""
	inBlockquote := false
	for rawLine := range strings.Lines(body) {
		line := strings.TrimRight(rawLine, "\r\n")
		trimmed := strings.TrimSpace(line)
		if fenceMarker != "" {
			flushParagraph()
			if closesMarkdownFence(trimmed, fenceMarker) {
				fenceMarker = ""
			}
			out = append(out, "")
			continue
		}
		if fence := leadingMarkdownFence(trimmed); fence != "" {
			flushParagraph()
			fenceMarker = fence
			inBlockquote = false
			out = append(out, "")
			continue
		}
		if trimmed == "" {
			flushParagraph()
			inBlockquote = false
			out = append(out, "")
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			flushParagraph()
			inBlockquote = true
			out = append(out, "")
			continue
		}
		if inBlockquote {
			if strings.HasPrefix(trimmed, "#") {
				inBlockquote = false
			} else {
				out = append(out, "")
				continue
			}
		}
		paragraph = append(paragraph, line)
	}
	flushParagraph()
	return strings.Join(out, "\n")
}

func normalizeDeclarationLine(trimmed string) string {
	s := stripInlineCodeSpans(trimmed)
	if m := absorbBulletLead.FindStringIndex(s); m != nil {
		s = strings.TrimSpace(s[m[1]:])
	} else if strings.HasPrefix(s, "#") {
		s = strings.TrimSpace(strings.TrimLeft(s, "#"))
	}
	if m := commitPrefixLead.FindStringIndex(s); m != nil {
		s = strings.TrimSpace(s[m[1]:])
	}
	return s
}

func isAffirmativeLead(line string, leadStart int) bool {
	before := line[:leadStart]
	if i := strings.LastIndexAny(before, ".;"); i >= 0 {
		before = before[i+1:]
	}
	return affirmativeClause.MatchString(before)
}

// ParseClosingIssues extracts the issues (owner/repo#n) that body's closing
// keywords (Closes, Fixes, Resolves) name, including multi-issue lists and
// non-default-branch pull requests where GitHub omits closingIssuesReferences.
func ParseClosingIssues(selfRef, body string) []string {
	defaultRepo := repoOf(selfRef)
	var out []string
	addRef := func(r string) {
		if r == "" || strings.EqualFold(r, selfRef) {
			return
		}
		if !slices.ContainsFunc(out, func(existing string) bool { return strings.EqualFold(existing, r) }) {
			out = append(out, r)
		}
	}
	scanList := func(rest string) {
		cursor := 0
		first := true
		lastRepo := defaultRepo
		for _, loc := range shortIssueRef.FindAllStringSubmatchIndex(rest, -1) {
			matchStart := loc[0]
			if loc[10] >= 0 && matchStart < loc[10] {
				if c := rest[matchStart]; c != '#' && c != '/' {
					matchStart++
				}
			}
			gap := rest[cursor:matchStart]
			if first {
				if !closingFirstGap.MatchString(gap) {
					break
				}
			} else if !closingNextGap.MatchString(gap) {
				break
			}
			switch {
			case loc[2] >= 0 && loc[3] >= 0:
				lastRepo = rest[loc[2]:loc[3]]
				addRef(lastRepo + "#" + rest[loc[4]:loc[5]])
			case loc[6] >= 0 && loc[7] >= 0:
				lastRepo = rest[loc[6]:loc[7]]
				addRef(lastRepo + "#" + rest[loc[8]:loc[9]])
			case loc[10] >= 0 && loc[11] >= 0:
				if lastRepo == "" {
					return
				}
				addRef(lastRepo + "#" + rest[loc[10]:loc[11]])
			default:
				return
			}
			cursor = loc[1]
			first = false
		}
	}
	scanLine := func(trimmed string) {
		line := normalizeDeclarationLine(trimmed)
		for _, lead := range closingLead.FindAllStringIndex(line, -1) {
			if !isAffirmativeLead(line, lead[0]) {
				continue
			}
			scanList(line[lead[1]:])
		}
	}
	inCloseList := false
	for rawLine := range strings.Lines(sanitizeMarkdownBody(body)) {
		line := strings.TrimRight(rawLine, "\r\n")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			inCloseList = false
			continue
		}
		if !inCloseList && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")) {
			continue
		}
		if closingHeader.MatchString(trimmed) {
			inCloseList = true
			continue
		}
		if inCloseList {
			if m := absorbBulletLead.FindStringIndex(trimmed); m != nil {
				scanList(stripInlineCodeSpans(trimmed[m[1]:]))
				continue
			}
			inCloseList = false
		}
		scanLine(trimmed)
	}
	return out
}

// AppendClosingKeywords ensures body carries an explicit "Closes <issue-url>"
// line for each issue in issues (owner/repo#n or full issue URL) that body does
// not already close.
func AppendClosingKeywords(selfRef, body string, issues []string) string {
	existing := ParseClosingIssues(selfRef, body)
	var lines []string
	for _, raw := range issues {
		ref := strings.TrimSpace(raw)
		if strings.HasPrefix(ref, "https://github.com/") {
			ref = issueRefFromURL(ref)
		}
		repo, n, ok := strings.Cut(ref, "#")
		if !ok || repo == "" || n == "" || strings.EqualFold(ref, selfRef) {
			continue
		}
		if slices.ContainsFunc(existing, func(e string) bool { return strings.EqualFold(e, ref) }) {
			continue
		}
		existing = append(existing, ref)
		lines = append(lines, fmt.Sprintf("Closes https://github.com/%s/issues/%s", repo, n))
	}
	if len(lines) == 0 {
		return body
	}
	trimmed := strings.TrimRight(body, "\n")
	if trimmed == "" {
		return strings.Join(lines, "\n")
	}
	return trimmed + "\n\n" + strings.Join(lines, "\n")
}

// ParseAbsorbedPulls extracts the original pull requests (owner/repo#n) that a
// combining pull request explicitly supersedes or absorbs from its title and body.
func ParseAbsorbedPulls(selfRef, title, body string) []string {
	repo := repoOf(selfRef)
	var out []string
	addRef := func(r string) {
		if r == "" || strings.EqualFold(r, selfRef) {
			return
		}
		if !slices.ContainsFunc(out, func(existing string) bool { return strings.EqualFold(existing, r) }) {
			out = append(out, r)
		}
	}
	scanList := func(rest string) {
		cursor := 0
		first := true
		for _, loc := range shortPullMention.FindAllStringSubmatchIndex(rest, -1) {
			matchStart := loc[0]
			if loc[10] >= 0 && matchStart < loc[10] {
				if c := rest[matchStart]; c != '#' && c != 'p' && c != 'P' {
					matchStart++
				}
			}
			gap := rest[cursor:matchStart]
			if first {
				if !absorbFirstGap.MatchString(gap) {
					break
				}
			} else if !absorbNextGap.MatchString(gap) {
				break
			}
			switch {
			case loc[2] >= 0 && loc[3] >= 0:
				addRef(rest[loc[2]:loc[3]] + "#" + rest[loc[4]:loc[5]])
			case loc[6] >= 0 && loc[7] >= 0:
				addRef(rest[loc[6]:loc[7]] + "#" + rest[loc[8]:loc[9]])
			case loc[10] >= 0 && loc[11] >= 0:
				if repo == "" {
					return
				}
				addRef(repo + "#" + rest[loc[10]:loc[11]])
			default:
				// An /issues/<n> link is not an absorbed pull request.
				return
			}
			cursor = loc[1]
			first = false
		}
	}
	scanLine := func(trimmed string) {
		line := normalizeDeclarationLine(trimmed)
		for _, lead := range absorbLead.FindAllStringIndex(line, -1) {
			if !isAffirmativeLead(line, lead[0]) {
				continue
			}
			scanList(line[lead[1]:])
		}
	}
	scanLine(strings.TrimSpace(title))
	inCombineList := false
	for rawLine := range strings.Lines(sanitizeMarkdownBody(body)) {
		line := strings.TrimRight(rawLine, "\r\n")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			inCombineList = false
			continue
		}
		if !inCombineList && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")) {
			continue
		}
		if absorbHeader.MatchString(trimmed) {
			inCombineList = true
			continue
		}
		if inCombineList {
			if m := absorbBulletLead.FindStringIndex(trimmed); m != nil {
				scanList(stripInlineCodeSpans(trimmed[m[1]:]))
				continue
			}
			inCombineList = false
		}
		scanLine(trimmed)
	}
	return out
}

// absorbedByMerged maps each lower-cased pull request ref absorbed by a merged
// combining pull request in in.Pulls to the combining pull request's ref.
func absorbedByMerged(in Input) map[string]string {
	out := map[string]string{}
	refs := make([]string, 0, len(in.Pulls))
	for ref := range in.Pulls {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		p := in.Pulls[ref]
		if p.State != Merged {
			continue
		}
		combiningRef := cmp.Or(p.Ref, ref)
		absorbs := p.Absorbs
		if len(absorbs) == 0 && p.Title != "" {
			absorbs = ParseAbsorbedPulls(combiningRef, p.Title, "")
		}
		for _, orig := range absorbs {
			key := strings.ToLower(orig)
			if key != "" && !strings.EqualFold(key, combiningRef) && out[key] == "" {
				out[key] = combiningRef
			}
		}
	}
	return out
}
