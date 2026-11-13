package superloop

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// A pull request another desk's worker opened has no live owner on this
// desk: that worker parks on its own desk, which the loop never reads. The
// claim the other desk publishes on the issue the pull request serves says
// whose it is (Input.Elsewhere), and its green head is that worker's
// request for this desk's review, which waits for no OrphanWait.

// AwaitingFirstReview are the pull requests of in.Ownerless, sorted, whose
// green head waits for the desk's first review: the ones a claim of another
// desk would have the loop review at once. The loop reads the claims of
// these only.
func AwaitingFirstReview(in Input) []string {
	l := newLedger(in.Taken, in.Answers)
	var out []string
	for ref := range in.Ownerless {
		if p, ok := in.Pulls[ref]; ok && awaitsFirstReview(l, ref, p) {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// awaitsFirstReview reports whether p is open and ready for a review and
// its green head has none yet: no conflict, no red check, no changes
// request of its owning team, and no review round or go of the desk.
func awaitsFirstReview(l ledger, ref string, p Pull) bool {
	if p.State != Open || p.Head == "" || p.Draft || !p.Green || len(p.Red) > 0 {
		return false
	}
	if p.Mergeable == mergeableConflicting || p.MergeState == mergeStateDirty || ownerChangesRequested(p) {
		return false
	}
	return len(l.rounds[ref]) == 0 && !l.has(reviewKey(ref, p.Head)) && !l.has(goKey(ref, p.Head))
}

// PullIssues are the issues (owner/repo#n) p serves: the ones it closes and
// the ones it says it is part of. A desk's claim of one of them is the claim
// of p's worker.
func PullIssues(ref string, p Pull) []string {
	out := slices.Clone(p.Closes)
	for _, issue := range ParsePartOf(ref, p.Body) {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, issue) }) {
			out = append(out, issue)
		}
	}
	return out
}

var (
	// partOfLead opens a line that names the issues a pull request is part
	// of, as a list item or not: "Part of https://github.com/o/r/issues/1".
	partOfLead = regexp.MustCompile(`(?i)^(?:[-*+]\s+)?(?:\*\*)?part\s+of\b(?:\*\*)?:?`)
	// partOfRef is one issue a "Part of" line names: its URL, owner/repo#n,
	// or #n in selfRef's repository.
	partOfRef = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/issues/(\d+)\b|\b([\w.-]+/[\w.-]+)#(\d+)\b|(?:^|[\s,(])#(\d+)\b`)
)

// ParsePartOf is the issues (owner/repo#n) selfRef's body names on its
// "Part of" lines: a pull request of a task that spans several pull requests
// serves its issue without closing it.
func ParsePartOf(selfRef, body string) []string {
	var out []string
	add := func(r string) {
		if !strings.EqualFold(r, selfRef) && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, r) }) {
			out = append(out, r)
		}
	}
	for line := range strings.Lines(body) {
		lead := partOfLead.FindStringIndex(strings.TrimSpace(line))
		if lead == nil {
			continue
		}
		rest := strings.TrimSpace(line)[lead[1]:]
		for _, m := range partOfRef.FindAllStringSubmatch(rest, -1) {
			switch {
			case m[1] != "":
				add(m[1] + "#" + m[2])
			case m[3] != "":
				add(m[3] + "#" + m[4])
			case m[5] != "" && repoOf(selfRef) != "":
				add(repoOf(selfRef) + "#" + m[5])
			}
		}
	}
	return out
}
