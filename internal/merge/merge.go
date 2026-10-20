// Package merge is the gate's logic on devctl pr merge: which lane a merge
// queues in, what holds it, whose turn it is, and when a lane's installation
// has rolled the previous merge.
package merge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// Hold targets besides owner/repo and github.
const (
	// AllMerges holds every merge but the hold's Except.
	AllMerges = "merges"
	// LanePrefix prefixes a lane's name in a hold target.
	LanePrefix = "lane:"
	// ToolRepo is the merge tool's own repository: its merge opens the
	// tool-release window.
	ToolRepo = "giantswarm/devctl"
	// Tool is the merge tool's binary.
	Tool = "devctl"
)

// devctl pr merge's flags of a detached merge, which the gate drops.
const (
	detachFlag = "--detach"
	onDoneFlag = "--on-done"
)

var (
	repoArg = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	numArg  = regexp.MustCompile(`^\d+$`)
)

// ParseArgs finds the repository and number in a devctl pr merge argument
// vector; ok is false for anything else (--help, a missing number).
func ParseArgs(argv []string) (repo string, pr int, ok bool) {
	i := slices.Index(argv, "merge")
	if len(argv) < 3 || i < 2 || argv[i-1] != "pr" {
		return "", 0, false
	}
	for _, a := range argv[i+1:] {
		switch {
		case a == "-h" || a == "--help":
			return "", 0, false
		case repo == "" && repoArg.MatchString(a):
			repo = a
		case repo != "" && numArg.MatchString(a):
			n, err := strconv.Atoi(a)
			return repo, n, err == nil && n > 0
		}
	}
	return "", 0, false
}

// StripDetach drops devctl pr merge's --detach and --on-done (with its
// command, either form) from argv: the gate runs every merge outside its
// caller already, and a detached devctl would leave the gate's unit and its
// lane accounting. stripped lists what went; argv of any other command comes
// back as it is.
func StripDetach(argv []string) (out, stripped []string) {
	i := slices.Index(argv, "merge")
	if i < 1 || argv[i-1] != "pr" {
		return argv, nil
	}
	out = slices.Clip(argv[:i+1])
	for j := i + 1; j < len(argv); j++ {
		switch a := argv[j]; {
		case a == "--":
			return append(out, argv[j:]...), stripped
		case a == detachFlag || strings.HasPrefix(a, detachFlag+"="),
			strings.HasPrefix(a, onDoneFlag+"="):
			stripped = append(stripped, a)
		case a == onDoneFlag:
			stripped = append(stripped, a)
			if j+1 < len(argv) {
				j++
				stripped = append(stripped, argv[j])
			}
		default:
			out = append(out, a)
		}
	}
	return out, stripped
}

// noReleaseWaitFlag ends devctl pr merge at the merge.
const noReleaseWaitFlag = "--no-release-wait"

// NoReleaseWait is a devctl pr merge argument vector that ends at the merge:
// argv with --no-release-wait after its subcommand, unless it has it.
func NoReleaseWait(argv []string) []string {
	i := slices.Index(argv, "merge")
	if i < 0 || slices.Contains(argv, noReleaseWaitFlag) {
		return argv
	}
	return slices.Insert(slices.Clone(argv), i+1, noReleaseWaitFlag)
}

// rebaseFlag makes devctl pr merge a rebase merge, the upstream-line forks'.
const rebaseFlag = "--rebase"

// Rebase says whether a devctl pr merge argument vector is a rebase merge:
// --rebase (or --rebase=true) among its flags, before any --.
func Rebase(argv []string) bool {
	i := slices.Index(argv, "merge")
	if i < 1 || argv[i-1] != "pr" {
		return false
	}
	for _, a := range argv[i+1:] {
		switch a {
		case "--":
			return false
		case rebaseFlag, rebaseFlag + "=true":
			return true
		}
	}
	return false
}

// timeoutFlag bounds devctl pr merge's wait for the CI outcome.
const timeoutFlag = "--timeout"

// CITimeout is a devctl pr merge argument vector whose CI wait is bounded
// by d: argv with --timeout d after its subcommand, unless it names its own
// --timeout or d is zero.
func CITimeout(argv []string, d time.Duration) []string {
	i := slices.Index(argv, "merge")
	if i < 0 || d <= 0 || slices.ContainsFunc(argv, func(a string) bool {
		return a == timeoutFlag || strings.HasPrefix(a, timeoutFlag+"=")
	}) {
		return argv
	}
	return slices.Insert(slices.Clone(argv), i+1, timeoutFlag, d.String())
}

// versionFlag pins devctl release promote to one release candidate.
const versionFlag = "--version"

// ParsePromote finds the one repository of a devctl release promote
// argument vector and the candidate its --version pins, "" when it promotes
// whatever candidate is newest; ok is false for anything else (several
// repositories, --team, --dry-run, --help), which runs ungated.
func ParsePromote(argv []string) (repo, version string, ok bool) {
	i := slices.Index(argv, "promote")
	if i < 2 || argv[i-1] != releaseCmd {
		return "", "", false
	}
	for j := i + 1; j < len(argv); j++ {
		a := argv[j]
		switch {
		case a == "--progress" || strings.HasPrefix(a, "--log-level"):
		case a == versionFlag && j+1 < len(argv) && version == "":
			j++
			version = argv[j]
		case strings.HasPrefix(a, versionFlag+"=") && version == "":
			version = strings.TrimPrefix(a, versionFlag+"=")
		case strings.HasPrefix(a, "-") || repo != "" || !repoArg.MatchString(a):
			return "", "", false
		default:
			repo = a
		}
	}
	if repo == "" || strings.HasPrefix(version, "-") || slices.Contains(argv[i+1:], versionFlag+"=") {
		return "", "", false
	}
	return repo, version, true
}

// ParsePromoteDocument reads devctl release promote's JSON document: a
// dispatched candidate (vX.Y.Z-rc.N) is a merge whose release is its stable
// version (vX.Y.Z), a repository with nothing to promote warrants no
// release. ok is false when it is none.
func ParsePromoteDocument(raw []byte) (Outcome, bool) {
	var doc struct {
		Repositories []struct {
			Candidate string `json:"candidate"`
			State     string `json:"state"`
		} `json:"repositories"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Repositories) != 1 {
		return Outcome{}, false
	}
	r := doc.Repositories[0]
	o := Outcome{Merged: r.State == "dispatched", NoRelease: r.State == "nothing_to_promote", State: r.State}
	if o.Merged {
		o.Release, _, _ = strings.Cut(r.Candidate, "-")
	}
	return o, true
}

// ParsePromoteCandidate reads the candidate of devctl release promote
// --dry-run's JSON document for one repository: the release candidate a
// promotion would dispatch now, "" when there is none. ok is false when the
// document names no single repository.
func ParsePromoteCandidate(raw []byte) (candidate string, ok bool) {
	var doc struct {
		Repositories []struct {
			Candidate string `json:"candidate"`
		} `json:"repositories"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Repositories) != 1 {
		return "", false
	}
	return doc.Repositories[0].Candidate, true
}

// owned are the subcommands of devctl that block until an outcome, which
// the gate runs outside their caller so that the outcome reaches its owner.
var owned = [][2]string{{"pr", "merge"}, {"pr", waitVerb}, {releaseCmd, waitVerb}, {"rollout", waitVerb}}

const (
	waitVerb   = "wait"
	releaseCmd = "release"
)

// capped are the subcommands of devctl that count against merge.cap: the
// merges and promotes. The waits only poll and run outside it.
var capped = [][2]string{{"pr", "merge"}, {releaseCmd, "promote"}}

// ParseOwned reports whether argv is devctl pr merge, pr wait, release wait
// or rollout wait, not a help call.
func ParseOwned(argv []string) bool {
	return slices.Contains(owned, subcommand(argv))
}

// ParseCapped reports whether argv is devctl pr merge or release promote,
// not a help call or a dry run: a run that counts against merge.cap.
func ParseCapped(argv []string) bool {
	return !slices.Contains(argv, "--dry-run") && slices.Contains(capped, subcommand(argv))
}

// subcommand is the command and verb of a devctl argument vector, zero for
// a help call or a vector without both.
func subcommand(argv []string) [2]string {
	if slices.Contains(argv, "-h") || slices.Contains(argv, "--help") {
		return [2]string{}
	}
	for i := 1; i+1 < len(argv); i++ {
		if !strings.HasPrefix(argv[i], "-") {
			return [2]string{argv[i], argv[i+1]}
		}
	}
	return [2]string{}
}

// Outcome is what devctl's document says about the merge.
type Outcome struct {
	// Merged is true when the document names a merge commit.
	Merged bool
	// Release is the tag the merge released and devctl confirmed pullable,
	// empty when none or unknown.
	Release string
	// NoRelease is true when the merge warranted no release: nothing rolls.
	NoRelease bool
	// ReleaseFailed is true when the merge's release failed (devctl exit 6,
	// verdict ci_failed): nothing rolls. Release is its tag when it was cut.
	ReleaseFailed bool
	// Unconfirmed is true when GitHub, not the document, reports the merge:
	// devctl ended before it confirmed the release.
	Unconfirmed bool
	// State is a promotion's state in devctl release promote's document
	// (dispatched, nothing_to_promote, not_built, …), empty for a merge.
	State string
}

// Judged is the outcome of a run that ended without its document or by a
// signal (exit 128+n): GitHub's state of the pull request decides whether it
// merged, never the exit code, and a merge's release is unconfirmed.
func Judged(p github.Pull) Outcome {
	return Outcome{Merged: p.State == github.Merged, Unconfirmed: p.State == github.Merged}
}

// NeedsJudging says whether a run's outcome is GitHub's to decide: devctl
// printed no document (ok false) or a signal ended it.
func NeedsJudging(ok bool, rc int) bool {
	return !ok || rc > 128
}

// ParseDocument reads devctl pr merge's JSON document (docs/pr-merge.md in
// giantswarm/devctl): its release object carries the verdict and the tag side
// by side. The tag counts only with the verdict available, a release devctl
// confirmed pullable; any other tag is unknown and the lane settles by the
// settle rule. A failed release (ci_failed) keeps its tag, if any, and
// rolls nothing. ok is false when it is none.
func ParseDocument(raw []byte) (Outcome, bool) {
	var doc struct {
		MergeCommitSha string `json:"mergeCommitSha"`
		Release        *struct {
			Verdict string `json:"verdict"`
			Tag     string `json:"tag"`
		} `json:"release"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return Outcome{}, false
	}
	o := Outcome{Merged: doc.MergeCommitSha != ""}
	if doc.Release != nil {
		switch doc.Release.Verdict {
		case "available":
			o.Release = doc.Release.Tag
		case "no_release":
			o.NoRelease = true
		case "ci_failed":
			o.Release, o.ReleaseFailed = doc.Release.Tag, true
		}
	}
	return o, true
}

// ParseReleaseWait reads devctl release wait's JSON document as a merge's
// release: available is its tag, a failed tag pipeline or commit status
// (ci_failed, status_failed) a failed release that keeps its tag, any other
// verdict (timeout, ambiguous_release, …) unconfirmed. reason is devctl's;
// ok is false when the document is none.
func ParseReleaseWait(raw []byte) (o Outcome, reason string, ok bool) {
	var doc struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
		Tag     string `json:"tag"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Verdict == "" {
		return Outcome{}, "", false
	}
	o.Merged = true
	switch doc.Verdict {
	case "available":
		o.Release = doc.Tag
	case "ci_failed", "status_failed":
		o.Release, o.ReleaseFailed = doc.Tag, true
	default:
		o.Unconfirmed = true
	}
	return o, doc.Reason, true
}

// MergeCommit is the merge commit a merge's document (ParseDocument) names,
// "" when none.
func MergeCommit(raw []byte) string {
	var doc struct {
		MergeCommitSha string `json:"mergeCommitSha"`
	}
	_ = json.Unmarshal(raw, &doc)
	return doc.MergeCommitSha
}

// Blocking is the active hold that stops a merge of repo#pr in lane: the
// repository's, the lane's, github's or one on all merges, unless the hold
// lets that repository or pull request through, else an upgrade running on
// the lane's installation. pr 0 is the repository's promote, which a hold
// waiting on the repository's own release lets through (AwaitsOwnRelease):
// held, the release could never come. A tool-release window stops the other merges only
// once its release is coming (ToolReleased): until then the installed tool is
// the latest release and runs, and only from the release on does every new
// run of it refuse until it is updated.
func Blocking(st *state.State, now time.Time, repo string, pr int, lane config.Lane) (state.Hold, bool) {
	for _, h := range st.Holds {
		if !h.Active(now) || h.Excepts(repo, pr) || h.Tool != "" && !ToolReleased(h) || pr == 0 && h.Target == repo && h.AwaitsOwnRelease() {
			continue
		}
		switch h.Target {
		case repo, LanePrefix + lane.Name, "github", AllMerges:
			return h, true
		}
	}
	return upgrade.Held(st, lane.Installation, now)
}

// Unheld is present for a lane's places that no hold blocks: a waiting merge
// a hold keeps out of the lane holds no place against the merges behind it,
// so the one merge the hold lets through enters first, and the held place
// keeps its turn for after the hold.
func Unheld(st *state.State, now time.Time, lane config.Lane, present func(state.Merge) bool) func(state.Merge) bool {
	return func(m state.Merge) bool {
		_, held := Blocking(st, now, m.Repo, m.PR, lane)
		return !held && present(m)
	}
}

// Preempts is the hold that takes the running merge m back out of lane
// before its devctl merges: a hold set after m started that lets another
// pull request through (--except owner/repo#n), an order that pull request
// goes first in. A hold without such an exception stops new merges only.
func Preempts(st *state.State, now time.Time, m state.Merge, lane config.Lane) (state.Hold, bool) {
	h, held := Blocking(st, now, m.Repo, m.PR, lane)
	if !held || !h.At.After(m.Started) || !strings.Contains(h.Except, "#") {
		return state.Hold{}, false
	}
	return h, true
}

// FixWindow is the active hold on lane that lets exactly repo#pr through
// (hold set --lane <lane> --except <repo>#<pr>): a declared fix window, in
// which that pull request merges without the lane's installation Ready or
// its previous merge rolled, the conditions a broken rollout only the fix
// itself can restore.
func FixWindow(st *state.State, now time.Time, repo string, pr int, lane config.Lane) (state.Hold, bool) {
	for _, h := range st.Holds {
		if h.Active(now) && h.Target == LanePrefix+lane.Name && strings.EqualFold(h.Except, fmt.Sprintf("%s#%d", repo, pr)) {
			return h, true
		}
	}
	return state.Hold{}, false
}

// Prune drops the waiting merges whose gate is gone: a place a run that ended
// left behind (Finished set, as an older release kept one for the retry) at
// once, any other once its gate left more than ttl ago (seedTTL for a seeded
// place), and settles the lost ones (Lost).
func Prune(st *state.State, now time.Time, ttl, seedTTL time.Duration, alive func(pid int) bool) {
	st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
		if m.Phase != state.Waiting || alive(m.PID) {
			return false
		}
		keep := ttl
		if m.Seeded {
			keep = seedTTL
		}
		return !m.Finished.IsZero() || now.Sub(m.Seen) > keep
	})
	Lost(st, now, alive)
}

// Lost turns each running merge whose gate process and devctl are gone
// (killed, or lost with the machine) into a settling one and returns them:
// whether it merged is unknown, so the lane settles by the settle rule from
// now.
func Lost(st *state.State, now time.Time, alive func(pid int) bool) []state.Merge {
	var lost []state.Merge
	for i, m := range st.Merges {
		if m.Phase == state.Running && !Runs(m, alive) {
			st.Merges[i].Phase, st.Merges[i].Finished, st.Merges[i].Exit = state.Settling, now, -1
			st.Merges[i].Release, st.Merges[i].ReleaseFailed, st.Merges[i].Roll = "", false, nil
			lost = append(lost, m)
		}
	}
	return lost
}

// Runs says whether a running merge's gate or its devctl still runs.
func Runs(m state.Merge, alive func(pid int) bool) bool {
	return alive(m.PID) || alive(m.Child)
}

// Hung says whether a running merge whose devctl still runs has outlived
// its pull request p at now: closed without a merge, or merged more than
// after ago. devctl confirms a merge's release within its --timeout, so one
// running on past it holds its lane for nothing.
func Hung(p github.Pull, now time.Time, after time.Duration) bool {
	switch p.State {
	case github.Closed:
		return true
	case github.Merged:
		return now.Sub(p.MergedAt) > after
	}
	return false
}

// Lane is one lane's queue.
type Lane struct {
	Name    string       `json:"name"`
	Running *state.Merge `json:"running,omitempty"`
	// Settling is the latest of the lane's settling merges, AllSettling all
	// of them, oldest first: a merge outside the gate can settle while a
	// gated one does.
	Settling    *state.Merge   `json:"settling,omitempty"`
	AllSettling []*state.Merge `json:"allSettling,omitempty"`
	Waiting     []state.Merge  `json:"waiting"`
	// Failed are the lane's merges whose last run merged nothing, oldest
	// first, until their next attempt.
	Failed []state.Failed `json:"failed,omitempty"`
}

// Queue returns the lane's running and settling merges and the waiting
// ones in turn order: a settled outside merge first, then in join order.
func Queue(st *state.State, lane string) Lane {
	q := Lane{Name: lane, Waiting: []state.Merge{}}
	for i := range st.Merges {
		m := &st.Merges[i]
		if m.Lane != lane {
			continue
		}
		switch m.Phase {
		case state.Running:
			q.Running = m
		case state.Settling:
			q.AllSettling = append(q.AllSettling, m)
		default:
			q.Waiting = append(q.Waiting, *m)
		}
	}
	slices.SortStableFunc(q.AllSettling, func(a, b *state.Merge) int { return a.Finished.Compare(b.Finished) })
	if n := len(q.AllSettling); n > 0 {
		q.Settling = q.AllSettling[n-1]
	}
	turn := promoteTurns(st, q.Waiting)
	slices.SortStableFunc(q.Waiting, func(a, b state.Merge) int {
		if a.Outside != b.Outside {
			if a.Outside {
				return -1
			}
			return 1
		}
		if c := turn(a).Compare(turn(b)); c != 0 {
			return c
		}
		if ap, bp := isPromote(a), isPromote(b); ap != bp {
			if ap {
				return -1
			}
			return 1
		}
		return a.Joined.Compare(b.Joined)
	})
	for _, f := range st.Failed {
		if f.Lane == lane {
			q.Failed = append(q.Failed, f)
		}
	}
	return q
}

func isPromote(m state.Merge) bool { return m.PR == 0 }

// promoteTurns gives each waiting merge the time it takes its place in the
// lane by. While a tool-release window is open, a waiting promotion of the
// tool's repository goes before every waiting merge of that repository: the
// merges wait for the window, which closes only once the stable release is
// promoted and installed, so a promotion queued behind one would wait for the
// merge that waits for it. Any other merge keeps its join time.
func promoteTurns(st *state.State, waiting []state.Merge) func(state.Merge) time.Time {
	open := slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return h.Tool != "" })
	var first time.Time
	if open {
		for _, m := range waiting {
			if m.Repo == ToolRepo && !isPromote(m) && (first.IsZero() || m.Joined.Before(first)) {
				first = m.Joined
			}
		}
	}
	return func(m state.Merge) time.Time {
		if open && m.Repo == ToolRepo && isPromote(m) && !first.IsZero() && first.Before(m.Joined) {
			return first
		}
		return m.Joined
	}
}

// failedFor is how long a failed attempt stays on its lane without a next
// one: its pull request is fixed, closed or forgotten by then.
const failedFor = 7 * 24 * time.Hour

// Fail records f, a merge's run that merged nothing, in place of the pull
// request's earlier failed attempt, and drops the attempts older than
// failedFor.
func Fail(st *state.State, f state.Failed) {
	Retry(st, f.Repo, f.PR)
	st.Failed = slices.DeleteFunc(st.Failed, func(o state.Failed) bool { return f.At.Sub(o.At) > failedFor })
	st.Failed = append(st.Failed, f)
}

// Retry drops repo#pr's failed attempt: its next attempt joins the lane.
func Retry(st *state.State, repo string, pr int) {
	st.Failed = slices.DeleteFunc(st.Failed, func(f state.Failed) bool { return f.Repo == repo && f.PR == pr })
}

// failedConclusions are the check conclusions and CircleCI workflow states
// that fail a pull request's CI.
var failedConclusions = []string{"failure", "failed", "failing", "error", "cancelled", "canceled", "timed_out", "action_required", "startup_failure"}

// FailedChecks are the names of the checks and CircleCI workflows devctl's
// document reports failed, in its order, each once (an Actions run's failed
// jobs are checks of their own).
func FailedChecks(doc []byte) []string {
	type run struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	}
	var d struct {
		Checks   []run `json:"checks"`
		CircleCI *struct {
			Workflows []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"workflows"`
		} `json:"circleci"`
	}
	if json.Unmarshal(doc, &d) != nil {
		return nil
	}
	all := d.Checks
	if d.CircleCI != nil {
		for _, w := range d.CircleCI.Workflows {
			all = append(all, run{Name: "circleci workflow " + w.Name, Conclusion: w.Status})
		}
	}
	var names []string
	for _, r := range all {
		if slices.Contains(failedConclusions, strings.ToLower(r.Conclusion)) && !slices.Contains(names, r.Name) {
			names = append(names, r.Name)
		}
	}
	return names
}

// serverError is a 5xx GitHub answered one of devctl's writes with: the
// merge or the branch update, which devctl sends once (go-github's
// "PUT <url>/pulls/<n>/merge: 502 Bad Gateway").
var serverError = regexp.MustCompile(`/pulls/\d+/(?:merge|update-branch): 5\d\d\b`)

// ServerError says whether devctl's document is a tooling failure (exit 7)
// for a 5xx on its merge or branch-update call: GitHub failed in transit,
// and the same merge sent again may well land.
func ServerError(doc []byte) bool {
	var d struct {
		ExitCode int    `json:"exitCode"`
		Reason   string `json:"reason"`
	}
	return json.Unmarshal(doc, &d) == nil && d.ExitCode == 7 && serverError.MatchString(d.Reason)
}

// Present says whether a waiting merge holds its place against the arrived
// merges behind it: a merge run outside the gate, one whose devctl pr merge
// is in the gate, or was within ttl (a rerun after exit 76, the retry of a
// failed attempt). A seeded place whose merge has not arrived, or left more
// than ttl ago, is absent.
func Present(m state.Merge, now time.Time, ttl time.Duration, alive func(pid int) bool) bool {
	return m.Outside || alive(m.PID) || (m.PID != 0 && now.Sub(m.Seen) <= ttl)
}

// Ahead is the waiting merge repo#pr waits behind, false when repo#pr is
// the lane's next: the first merge before it that is present, or, for a
// seeded place, an absent seed before it, as seeds keep their order, unless
// that seed is a later pull request of the same session and repository,
// which cannot arrive first. A free lane runs the first arrived merge.
func (q Lane) Ahead(repo string, pr int, present func(state.Merge) bool) (state.Merge, bool) {
	i := slices.IndexFunc(q.Waiting, func(m state.Merge) bool { return m.Repo == repo && m.PR == pr })
	if i < 0 {
		return state.Merge{}, false
	}
	me := q.Waiting[i]
	for _, m := range q.Waiting[:i] {
		if holds(m, me, present) {
			return m, true
		}
	}
	return state.Merge{}, false
}

// SessionTurn says whether the waiting merge me, its lane's next, may start
// under the cap (most) on one party's running merges: each running devctl polls
// GitHub, a waiting place does not. It returns the party's running merges
// (gate or devctl alive) and, where me waits, the party's merge that takes the
// free slot first: one that joined before me, is present and is free to start
// (its lane's next, nothing running or settling in its lane), so the party's
// merges start in the order they queued. most 0 or less is no cap.
func SessionTurn(st *state.State, me state.Merge, most int, present func(state.Merge) bool, alive func(pid int) bool) (running []state.Merge, first *state.Merge, ok bool) {
	if most <= 0 {
		return nil, nil, true
	}
	var earlier []state.Merge
	for _, m := range st.Merges {
		if !m.By.Is(me.By) || m.Repo == me.Repo && m.PR == me.PR {
			continue
		}
		switch {
		case m.Phase == state.Running && Runs(m, alive):
			running = append(running, m)
		case m.Phase == state.Waiting && m.Joined.Before(me.Joined) && present(m) && free(st, m, present):
			earlier = append(earlier, m)
		}
	}
	slices.SortStableFunc(earlier, func(a, b state.Merge) int { return a.Joined.Compare(b.Joined) })
	slots := most - len(running)
	if slots <= 0 {
		return running, nil, false
	}
	if len(earlier) >= slots {
		return running, &earlier[0], false
	}
	return running, nil, true
}

// InResetWindow says whether m was queued under the GitHub budget floor for
// a reset that came less than window ago.
func InResetWindow(m state.Merge, now time.Time, window time.Duration) bool {
	return !m.Reset.IsZero() && !now.Before(m.Reset) && now.Before(m.Reset.Add(window))
}

// ResetFirst are the merges that go first at the GitHub budget's reset,
// oldest queued first: each waits in a live gate, queued under the floor
// for a reset that came less than window ago. The waits yield to them until
// they start or their window ends.
func ResetFirst(st *state.State, now time.Time, window time.Duration, alive func(pid int) bool) []state.Merge {
	var out []state.Merge
	for _, m := range st.Merges {
		if m.Phase == state.Waiting && InResetWindow(m, now, window) && alive(m.PID) {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b state.Merge) int { return a.Joined.Compare(b.Joined) })
	return out
}

// ResetAhead are the merges of ResetFirst queued before me that are free to
// start (their lane's next, nothing running or settling in it): each takes a
// devctl slot before me.
func ResetAhead(st *state.State, me state.Merge, now time.Time, window time.Duration, present func(state.Merge) bool, alive func(pid int) bool) []state.Merge {
	var out []state.Merge
	for _, m := range ResetFirst(st, now, window, alive) {
		if m.Joined.Before(me.Joined) && m.Key() != me.Key() && free(st, m, present) {
			out = append(out, m)
		}
	}
	return out
}

// ResetFirstUntil is when the last window of first ends.
func ResetFirstUntil(first []state.Merge, window time.Duration) time.Time {
	var until time.Time
	for _, m := range first {
		if end := m.Reset.Add(window); end.After(until) {
			until = end
		}
	}
	return until
}

// Keys are the merges' keys, comma-separated.
func Keys(ms []state.Merge) string {
	keys := make([]string, len(ms))
	for i, m := range ms {
		keys[i] = m.Key()
	}
	return strings.Join(keys, ", ")
}

// free says whether the waiting merge m is its lane's next with nothing
// running or settling in the lane.
func free(st *state.State, m state.Merge, present func(state.Merge) bool) bool {
	q := Queue(st, m.Lane)
	_, behind := q.Ahead(m.Repo, m.PR, present)
	return !behind && q.Running == nil && q.Settling == nil
}

// holds says whether the waiting merge m, before me in the queue, holds me
// up: m is present, or both are seeded places and m is not a later pull
// request of me's session and repository.
func holds(m, me state.Merge, present func(state.Merge) bool) bool {
	later := m.Repo == me.Repo && m.PR > me.PR && m.By.Name == me.By.Name
	return present(m) || (me.Seeded && m.Seeded && !later)
}

// Stall is a lane's first arrived merge held up by places whose merges are
// not in the gate.
type Stall struct {
	// Merge is the waiting merge whose gate call is in the gate.
	Merge state.Merge `json:"merge"`
	// Behind are the places before it that hold it up: seeds whose merges
	// have not arrived, merges that left the gate.
	Behind []state.Merge `json:"behind"`
	// Since is when the wait began: the gate call's arrival, the last of
	// those places going absent or the lane's last merge ending, the latest.
	Since time.Time `json:"since"`
}

// Stalled reports the lane's stall: no merge runs, the first waiting merge
// whose gate call is in the gate (alive) is held up only by places whose
// merges are not, none of them run outside the gate, and it has waited so
// for longer than after. arrived is when a gate call's process started.
func (q Lane) Stalled(now time.Time, after time.Duration, present func(state.Merge) bool, alive func(pid int) bool,
	arrived func(pid int) (time.Time, bool)) (Stall, bool) {
	if q.Running != nil {
		return Stall{}, false
	}
	i := slices.IndexFunc(q.Waiting, func(m state.Merge) bool { return alive(m.PID) })
	if i < 0 {
		return Stall{}, false
	}
	s := Stall{Merge: q.Waiting[i]}
	since, ok := arrived(s.Merge.PID)
	if !ok {
		return Stall{}, false
	}
	for _, m := range q.Waiting[:i] {
		if !holds(m, s.Merge, present) {
			continue
		}
		if m.Outside {
			return Stall{}, false
		}
		gone := m.Joined
		if m.PID != 0 {
			gone = m.Seen
		}
		since = latest(since, gone)
		s.Behind = append(s.Behind, m)
	}
	if q.Settling != nil {
		since = latest(since, q.Settling.Finished)
	}
	if len(s.Behind) == 0 || now.Sub(since) <= after {
		return Stall{}, false
	}
	s.Since = since
	return s, true
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Passed names the absent merges before repo#pr that it runs ahead of.
func (q Lane) Passed(repo string, pr int) string {
	var keys []string
	for _, m := range q.Waiting {
		if m.Repo == repo && m.PR == pr {
			break
		}
		keys = append(keys, m.Key())
	}
	return strings.Join(keys, " ")
}

// Merged turns an outside merge that GitHub reports merged at into its
// lane's settling merge. Its release is unknown: the lane settles by the
// settle rule from the merge on.
func Merged(m *state.Merge, at time.Time) {
	m.Phase, m.Finished, m.PID, m.Seeded = state.Settling, at.UTC(), 0, false
	m.Release, m.ReleaseFailed, m.Roll, m.Checked = "", false, nil, time.Time{}
}

// SettlingKeys names the lane's settling merges, oldest first.
func (q Lane) SettlingKeys() string {
	keys := make([]string, 0, len(q.AllSettling))
	for _, m := range q.AllSettling {
		keys = append(keys, m.Key())
	}
	return strings.Join(keys, " ")
}

// Position is the 1-based turn of repo#pr among the lane's waiting merges,
// 0 when it is not waiting.
func (q Lane) Position(repo string, pr int) int {
	for i, m := range q.Waiting {
		if m.Repo == repo && m.PR == pr {
			return i + 1
		}
	}
	return 0
}

// HelmRelease is the part of a Flux HelmRelease the lane's readiness reads.
type HelmRelease struct {
	Key     string // namespace/name
	Chart   string
	Version string // the chart version without build metadata
	Ready   bool
	Message string
	// Source is the namespace/name of the OCIRepository the chart comes
	// from (spec.chartRef), "" for any other source.
	Source string
	// Range is the semver range the HelmRelease follows: its OCIRepository's
	// ref (semver, or a tag as an exact version) or its chart template's
	// version. "" is unknown: every release counts as followed.
	Range string
}

// ParseHelmReleases reads `kubectl get helmreleases -A -o json`.
func ParseHelmReleases(raw []byte) ([]HelmRelease, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Chart struct {
					Spec struct {
						Chart   string `json:"chart"`
						Version string `json:"version"`
					} `json:"spec"`
				} `json:"chart"`
				ChartRef struct {
					Kind      string `json:"kind"`
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"chartRef"`
			} `json:"spec"`
			Status struct {
				History []struct {
					ChartName    string `json:"chartName"`
					ChartVersion string `json:"chartVersion"`
				} `json:"history"`
				LastAttemptedRevision string `json:"lastAttemptedRevision"`
				Conditions            []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("reading the HelmReleases: %w", err)
	}
	out := make([]HelmRelease, 0, len(list.Items))
	for _, it := range list.Items {
		hr := HelmRelease{Key: it.Metadata.Namespace + "/" + it.Metadata.Name, Chart: it.Spec.Chart.Spec.Chart,
			Range: it.Spec.Chart.Spec.Version, Message: "no Ready condition yet"}
		if ref := it.Spec.ChartRef; ref.Kind == "OCIRepository" {
			ns := ref.Namespace
			if ns == "" {
				ns = it.Metadata.Namespace
			}
			hr.Source = ns + "/" + ref.Name
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				hr.Ready, hr.Message = c.Status == "True", c.Message
			}
		}
		// The rolled version is the newest release of the history; without
		// one, the attempted revision once it is Ready.
		switch {
		case len(it.Status.History) > 0:
			hr.Chart, hr.Version = it.Status.History[0].ChartName, Bare(it.Status.History[0].ChartVersion)
		case hr.Ready:
			hr.Version = Bare(it.Status.LastAttemptedRevision)
		}
		out = append(out, hr)
	}
	return out, nil
}

// AttachRanges sets the Range of each HelmRelease with an OCIRepository
// source from `kubectl get ocirepositories -A -o json`: the ref's semver
// range, or its tag as the one version it follows. A digest ref or a
// source that is not listed leaves the range unknown.
func AttachRanges(hrs []HelmRelease, raw []byte) error {
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Ref struct {
					SemVer string `json:"semver"`
					Tag    string `json:"tag"`
					Digest string `json:"digest"`
				} `json:"ref"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("reading the OCIRepositories: %w", err)
	}
	ranges := map[string]string{}
	for _, it := range list.Items {
		ref := it.Spec.Ref
		if ref.Digest != "" {
			continue
		}
		r := ref.SemVer
		if r == "" && ref.Tag != "" {
			r = "=" + Bare(ref.Tag)
		}
		ranges[it.Metadata.Namespace+"/"+it.Metadata.Name] = r
	}
	for i := range hrs {
		if hrs[i].Source != "" {
			hrs[i].Range = ranges[hrs[i].Source]
		}
	}
	return nil
}

// follows says whether a HelmRelease on range rng ever runs release. Flux
// picks versions with the same semver library, so a stable range excludes
// every release candidate. A range or release that does not parse, or an
// unknown range, counts as followed: the lane waits as before.
func follows(rng, release string) bool {
	c, errC := semver.NewConstraint(rng)
	v, errV := semver.NewVersion(Bare(release))
	if rng == "" || errC != nil || errV != nil {
		return true
	}
	return c.Check(v)
}

// PreRelease says whether release is a semver pre-release, a release
// candidate: an installation on a stable range never runs it.
func PreRelease(release string) bool {
	v, err := semver.NewVersion(Bare(release))
	return err == nil && v.Prerelease() != ""
}

// reached says whether a HelmRelease on version have runs release or a
// later one in semver order. A merge cuts a release candidate: an
// installation on a stable range runs its promotion, X.Y.Z after X.Y.Z-rc.N,
// never the candidate itself. Versions that are not semver compare equal or
// not at all.
func reached(have, release string) bool {
	h, errH := semver.NewVersion(Bare(have))
	r, errR := semver.NewVersion(Bare(release))
	if errH != nil || errR != nil {
		return Bare(have) == Bare(release)
	}
	return !h.LessThan(r)
}

// ToolReleased says the tool-release window h waits for a release that is
// coming: its pull request merged, or it is a promotion's, which dispatches
// one.
func ToolReleased(h state.Hold) bool { return h.Tool != "" && (h.ToolMerged || h.ToolPR == 0) }

// Installed says whether the merge tool reporting version v ends the
// tool-release window h: a window whose merge released waits for the tool to
// report that release or a later one, any other for a version other than the
// one it opened on.
func Installed(h state.Hold, v string) bool {
	if h.ToolMerged && h.ToolRelease != "" {
		return reached(v, h.ToolRelease)
	}
	return Bare(v) != Bare(h.ToolFrom)
}

// Bare is a version without a leading v and without build metadata: a tag
// v1.2.3 and a chart version 1.2.3+1c161d9d name the same release.
func Bare(v string) string {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	return v
}

func chartOf(repo string) string { return repo[strings.LastIndex(repo, "/")+1:] }

// laneReleases are the installation's HelmReleases of the lane's charts.
func laneReleases(lane config.Lane, hrs []HelmRelease) []HelmRelease {
	var out []HelmRelease
	for _, hr := range hrs {
		for _, r := range lane.Repositories {
			if strings.EqualFold(hr.Chart, chartOf(r)) {
				out = append(out, hr)
				break
			}
		}
	}
	return out
}

// RollSet names the HelmReleases a merge of repo must roll: those of its
// chart on the newest version when the merge starts. One pinned to an older
// version does not follow releases and is not waited for.
func RollSet(hrs []HelmRelease, repo string) []string {
	var newest *semver.Version
	var mine []HelmRelease
	for _, hr := range hrs {
		if !strings.EqualFold(hr.Chart, chartOf(repo)) {
			continue
		}
		mine = append(mine, hr)
		if v, err := semver.NewVersion(hr.Version); err == nil && (newest == nil || v.GreaterThan(newest)) {
			newest = v
		}
	}
	var out []string
	for _, hr := range mine {
		if v, err := semver.NewVersion(hr.Version); newest == nil || (err == nil && v.Equal(newest)) {
			out = append(out, hr.Key)
		}
	}
	return out
}

// Ready says whether the lane is free for its next merge: every HelmRelease
// of its charts Ready and the settling merge rolled. A settling merge with
// a known release has rolled when each HelmRelease of its roll set reports
// that release or a later one (reached), or follows a range that never
// admits it (a release candidate under a stable range); one whose release
// is unknown (merged without a confirmed release, or its run lost) has
// settled once settle has passed since it ended; one whose release failed
// rolls nothing and has settled at once. why says what the lane waits for;
// on a free lane it names why the merge settled without a roll, "" when it
// rolled.
func Ready(lane config.Lane, hrs []HelmRelease, settling *state.Merge, now time.Time, settle time.Duration) (bool, string) {
	mine := laneReleases(lane, hrs)
	var unfollowed []string
	switch {
	case settling == nil:
	case settling.ReleaseFailed:
		// Nothing rolls: only the lane's HelmReleases must be Ready.
	case settling.Release == "":
		if until := settling.Finished.Add(settle); now.Before(until) {
			return false, fmt.Sprintf("%s's release is unknown: the lane settles until %s", settling.Key(), until.Local().Format("15:04"))
		}
	default:
		for _, key := range settling.Roll {
			i := slices.IndexFunc(mine, func(hr HelmRelease) bool { return hr.Key == key })
			switch {
			case i < 0:
				return false, fmt.Sprintf("HelmRelease %s is gone from %s", key, lane.Installation)
			case !follows(mine[i].Range, settling.Release):
				unfollowed = append(unfollowed, fmt.Sprintf("%s follows semver %s", key, mine[i].Range))
			case !reached(mine[i].Version, settling.Release):
				why := fmt.Sprintf("%s is on %s, rolling to %s", key, mine[i].Version, Bare(settling.Release))
				if mine[i].Range != "" {
					why += fmt.Sprintf(" (semver %s)", mine[i].Range)
				}
				return false, why
			}
		}
	}
	for _, hr := range mine {
		if !hr.Ready {
			return false, fmt.Sprintf("%s is not Ready: %s", hr.Key, strings.TrimSpace(hr.Message))
		}
	}
	switch {
	case settling != nil && settling.ReleaseFailed:
		return true, fmt.Sprintf("%s's release failed, nothing rolls", settling.Key())
	case len(unfollowed) > 0:
		return true, fmt.Sprintf("%s does not follow %s: %s", lane.Installation, Bare(settling.Release), strings.Join(unfollowed, "; "))
	}
	return true, ""
}
