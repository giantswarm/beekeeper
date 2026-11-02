package superloop

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Desk is whose pull requests the loop acts on: the policy every pull
// request passes before a review, a go, a forward or an escalation.
type Desk struct {
	// Since is the loop's first start: a park from before it is no request
	// for a review.
	Since time.Time
	// Orgs are github.allowedOrgs: outside them the loop does nothing at
	// all. Empty: every owner.
	Orgs []string
	// Authors are the logins the desk opens its pull requests as; empty:
	// anybody's.
	Authors []string
	// Team is the desk's team as the team files name it; empty: every
	// repository in Orgs is the desk's.
	Team string
	// Review are the repositories (path.Match patterns over owner/name) the
	// desk reviews whoever owns them; Never the ones it does no work on,
	// whatever Review or an override says.
	Review, Never []string
	// OwnerWait is how long a worker waits for the owning team's review
	// before the person is asked to override it.
	OwnerWait time.Duration
	// TeamsUnknown says the team files could not be read: only Review and
	// the person's overrides say what is the desk's, and nothing else is
	// acted on until they are read.
	TeamsUnknown bool
}

// The reasons the loop leaves a pull request alone, in its leave's key.
const (
	leftOutside = "outside"
	leftNever   = "never"
	leftBefore  = "before"
	leftAuthor  = "author"
	leftTeam    = "team"
	leftFiles   = "files"
)

// owner is whose review a pull request waits for, as the desk sees it.
type owner int

const (
	// ours is the desk's: the loop reviews it and gives the go.
	ours owner = iota
	// theirs is another team's, or no known team's: its review is that
	// team's, and only the person's override makes it the desk's.
	theirs
	// nobody's is none the desk acts on at all.
	nobodys
	// unknown is one whose owner the desk cannot tell yet: the team files
	// are unread. It waits, quietly; the loop says why once.
	unknown
)

// Outside reports whether ref's owner is outside the desk's organisations.
func (d Desk) Outside(ref string) bool {
	if len(d.Orgs) == 0 {
		return false
	}
	org, _, _ := strings.Cut(ref, "/")
	return !slices.ContainsFunc(d.Orgs, func(o string) bool { return strings.EqualFold(o, org) })
}

// unread is why the loop leaves ref alone without reading it (its reason
// and its words), "" when it may read it: outside Orgs or in Never.
func (d Desk) unread(ref string) (reason, why string) {
	repo := repoOf(ref)
	switch {
	case d.Outside(ref):
		return leftOutside, fmt.Sprintf("%s is outside github.allowedOrgs (%s): the desk reviews, approves and writes nothing there", repo, strings.Join(d.Orgs, ", "))
	case matchesAny(d.Never, repo):
		return leftNever, fmt.Sprintf("%s is in supervisor.ownership.never: the desk does no work on it", repo)
	}
	return "", ""
}

// early reports whether a park at parkedAt is from before the loop's first
// start, and says so.
func (d Desk) early(parkedAt time.Time) (bool, string) {
	if !parkedAt.Before(d.Since) {
		return false, ""
	}
	return true, fmt.Sprintf("parked at %s, before the loop's first start at %s: a park from before is no request for its review",
		parkedAt.UTC().Format(time.RFC3339), d.Since.UTC().Format(time.RFC3339))
}

// owner is whose review p waits for, and when the loop leaves it alone,
// why (its reason and its words). overridden is the person's override of
// p's ownership, which never outranks Never, Orgs or Authors.
func (d Desk) owner(p Pull, overridden bool) (o owner, reason, why string) {
	repo := repoOf(p.Ref)
	if reason, why := d.unread(p.Ref); reason != "" {
		return nobodys, reason, why
	}
	if n := d.namedNever(p); n != "" {
		return nobodys, leftNever, fmt.Sprintf("its title, branch or changes name %s of supervisor.ownership.never: the desk does no work on it", n)
	}
	if len(d.Never) > 0 && p.ChangedFiles > len(p.Files) {
		return nobodys, leftFiles, fmt.Sprintf("it changes %d files and gh lists %d: the rest cannot be checked against supervisor.ownership.never", p.ChangedFiles, len(p.Files))
	}
	if len(d.Authors) > 0 && !slices.ContainsFunc(d.Authors, func(a string) bool { return strings.EqualFold(a, p.Author) }) {
		return nobodys, leftAuthor, fmt.Sprintf("opened by %s, not by the desk (%s): it is not the desk's to review", p.Author, strings.Join(d.Authors, ", "))
	}
	if overridden || d.Team == "" || matchesAny(d.Review, repo) {
		return ours, "", ""
	}
	if d.TeamsUnknown {
		return unknown, "", ""
	}
	if strings.EqualFold(p.Team, d.Team) {
		return ours, "", ""
	}
	return theirs, leftTeam, fmt.Sprintf("%s: its review is theirs; the loop starts no reviewer and gives no go", ownedBy(p.Team, repo))
}

// ownedBy says who owns repo: team, or no team file.
func ownedBy(team, repo string) string {
	if team == "" {
		return fmt.Sprintf("no team file declares %s, so no team of the desk's owns it", repo)
	}
	return fmt.Sprintf("%s owns %s", team, repo)
}

// teamName is team, or a word for none.
func teamName(team string) string {
	if team == "" {
		return "its owners"
	}
	return team
}

// namedNever is the Never pattern p's title, head branch or a changed path
// names, "" when none does.
func (d Desk) namedNever(p Pull) string {
	texts := append([]string{p.Title, p.Branch}, p.Files...)
	for _, n := range d.Never {
		// A pattern whose name is all wildcard (org/*) names no repository
		// in a text: it would match every title. unread covers its
		// repositories.
		_, name, _ := strings.Cut(n, "/")
		if strings.Trim(name, "*?") == "" {
			continue
		}
		re := nameWord(n)
		if slices.ContainsFunc(texts, re.MatchString) {
			return n
		}
	}
	return ""
}

// nameWord matches the repository name of pattern (owner/name) as a word of
// a text: not inside a longer run of letters and digits. * and ? match
// what a repository name may hold.
func nameWord(pattern string) *regexp.Regexp {
	_, name, _ := strings.Cut(pattern, "/")
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '*':
			b.WriteString(`[a-z0-9._-]*`)
		case '?':
			b.WriteString(`[a-z0-9._-]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return regexp.MustCompile(`(?i)(^|[^a-z0-9])` + b.String() + `($|[^a-z0-9])`)
}

// matchesAny reports whether repo (owner/name) matches one of patterns,
// without regard to case.
func matchesAny(patterns []string, repo string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool {
		ok, _ := path.Match(strings.ToLower(p), strings.ToLower(repo))
		return ok
	})
}

// repoOf is ref's owner/repo.
func repoOf(ref string) string {
	repo, _, _ := strings.Cut(ref, "#")
	return repo
}

// ownerReviews are p's reviews by a member or collaborator of its owner
// other than its author and no bot, submitted after since: the owning
// team's. Anybody else's review is no word of theirs.
func ownerReviews(p Pull, since time.Time) []Review {
	var out []Review
	for _, r := range p.Reviews {
		if r.Member && r.Author != "" && !strings.EqualFold(r.Author, p.Author) && !strings.HasSuffix(r.Author, "[bot]") && r.At.After(since) {
			out = append(out, r)
		}
	}
	return out
}

// ownerApproved reports whether the latest verdict of somebody other than
// p's author approves it.
func ownerApproved(p Pull) bool {
	reviews := ownerReviews(p, time.Time{})
	for i := len(reviews) - 1; i >= 0; i-- {
		if v := reviews[i].Verdict; v != "" {
			return v == Approved
		}
	}
	return false
}

// leaveKey, relayKey and ownershipKey are the keys of a leave, a relayed
// review and the person's decision on a pull request's ownership.
func leaveKey(ref, reason string) string { return fmt.Sprintf("leave %s %s", ref, reason) }

// leftUnreviewed is the reason a green head past its approval waits for a
// new park: one leave line per head.
func leftUnreviewed(head string) string  { return "unreviewed@" + short(head) }
func relayKey(ref, review string) string { return fmt.Sprintf("relay %s %s", ref, review) }
func ownershipKey(ref string) string     { return "ownership " + ref }
func overrideKey(ref string) string      { return "override " + ref }

// ownershipPrefix opens every ownership decision's question: an answer to a
// note with it is the person's answer about a pull request's ownership.
const ownershipPrefix = "Override "

// OwnershipQuestion is the question the guide's person is asked when the
// owning team's review of url blocks the desk's work.
func OwnershipQuestion(url, team string) string {
	if team == "" {
		return fmt.Sprintf("%sthe ownership of %s, which no team file declares: the desk reviews and merges it?", ownershipPrefix, url)
	}
	return fmt.Sprintf("%s%s's ownership of %s: the desk reviews and merges it?", ownershipPrefix, team, url)
}

// OwnershipOptions are an ownership decision's choices: the first
// overrides it.
func OwnershipOptions(team string) []string {
	return []string{
		"Override: the supervisor loop reviews this pull request and gives the merge go, as for the desk's own",
		fmt.Sprintf("Wait: the worker stays parked for %s's review", teamName(team)),
	}
}

// overrides are the whole answers, in their own words, that override a
// pull request's ownership.
var overrides = []string{"override", "override it", "yes override", "yes override it", "ok override", "ok override it"}

// Overrides reports whether the person's answer to an ownership decision
// overrides it. It fails closed, as Releases does: only the first option's
// label ("Override", its words after it or not) or a bare override does.
func Overrides(answer string) bool {
	label, _, _ := strings.Cut(strings.TrimSpace(answer), " — ")
	if strings.EqualFold(strings.TrimSpace(label), "override") {
		return true
	}
	return slices.Contains(overrides, bareWords(answer))
}

// bareWords is s lower-cased, without punctuation, one space between words.
func bareWords(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.Map(func(r rune) rune {
		if strings.ContainsRune(".,!:;", r) {
			return ' '
		}
		return r
	}, s))), " ")
}

// relayMessage tells the worker of p the owning team's review r word for
// word.
func relayMessage(p Pull, r Review, team string) string {
	verdict := r.Verdict
	if verdict == "" {
		verdict = "commented"
	}
	return fmt.Sprintf("%s reviewed %s for %s, its owning team: %s, on head %s. The review, word for word:\n\n%s\n\nIt is %s's review, not the desk's: an approval lets your gated merge run, which still judges its reviews and checks; answer changes or questions, push, and once CI is green, run `cd \"<worktree>\" && beekeeper agents park --on <owner/repo#n> \"desk review\"` from the published worktree.",
		r.Author, p.URL, teamName(team), verdict, short(r.Head), r.Body, teamName(team))
}

// ownershipAnswerMessage tells the worker of p the person's answer an to
// the decision on p's ownership, which did not override it.
func ownershipAnswerMessage(p Pull, an Answer) string {
	return fmt.Sprintf("%s answered whether the desk takes %s over from %s (note #%d), word for word: %s\n\nThe loop does not read it as an override: %s's review decides; act as the answer says.",
		an.By, p.URL, teamName(p.Team), an.Note, an.Text, teamName(p.Team))
}
