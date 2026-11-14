package decide

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/history"
)

// Board statuses, normalized (letters only, lower case), so that the emoji the board puts after a name
// don't matter.
const (
	statusInbox      = "inbox"
	statusBacklog    = "backlog"
	statusUpNext     = "upnext"
	statusInProgress = "inprogress"
	statusBlocked    = "blockedwaiting"
	statusValidation = "validation"
	statusDone       = "done"

	kindEpic    = "epic"
	kindStory   = "story"
	kindTask    = "task"
	kindFeature = "feature"
	kindBug     = "bug"
	kindAlert   = "alert"

	// StatusInProgress is the board status a pick would set.
	StatusInProgress = "In Progress ⛏️"
	// PurposeImplement is the purpose of the run a pick would start.
	PurposeImplement = "implement"
	// WorkImplementable is the optIn.work entry for implementable items.
	WorkImplementable = "implementable"

	// Event types (contract §5.2).
	EventItemRemoved        = "item.removed"
	EventPhaseChanged       = "phase.changed"
	EventEligibilityChanged = "eligibility.changed"
	EventShadowPick         = "shadow.pick"
	EventShadowHeld         = "shadow.held"
	EventShadowAction       = "shadow.action"
	EventWaitStarted        = "wait.started"
	EventWaitWoken          = "wait.woken"
	EventWaitEnded          = "wait.ended"
	EventWaitDeadline       = "wait.deadline"
	EventWaitReobserved     = "wait.reobserved"
	EventWaitUnobserved     = "wait.unobserved"
	EventWaitOverdue        = "wait.overdue"

	// ReasonRetention is item.removed's reason when the retention of an Item that left by status passed.
	ReasonRetention = "retention"
)

// norm keeps the letters of a board option, lower case: "Up Next ➡️" → "upnext".
func norm(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) && r < unicode.MaxASCII {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func boardStatus(st *v1alpha1.ItemStatus) string {
	if st.Board == nil {
		return ""
	}
	return norm(st.Board.Status)
}

func boardKind(st *v1alpha1.ItemStatus) string {
	if st.Board == nil {
		return ""
	}
	return norm(st.Board.Kind)
}

// classOf maps the board Kind to the item's class.
func classOf(kind string) string {
	switch kind {
	case kindEpic:
		return v1alpha1.ClassEpic
	case kindStory, kindTask, kindFeature, kindBug:
		return v1alpha1.ClassImplementable
	case kindAlert:
		return v1alpha1.ClassIncident
	default:
		return v1alpha1.ClassIgnored
	}
}

// activeStatus reports whether a board status keeps an item in the tracked set.
func activeStatus(status, kind string) bool {
	switch status {
	case statusUpNext, statusInProgress, statusBlocked, statusValidation:
		return true
	case statusInbox:
		return kind == kindBug
	}
	return false
}

// ItemRef is the bare ref of an Item's issue (owner/repo#n), empty before the issue is observed.
func ItemRef(it *v1alpha1.Item) string {
	if it == nil || it.Status.Issue == nil {
		return ""
	}
	return issueRef(it.Status.Issue)
}

func issueRef(is *v1alpha1.ItemIssue) string {
	return fmt.Sprintf("%s#%d", is.Repo, is.Number)
}

// ChangeRef is the bare ref of a Change's PR (owner/repo#n), empty before the PR is observed.
func ChangeRef(c *v1alpha1.Change) string {
	if c == nil || c.Status.PR == nil {
		return ""
	}
	return fmt.Sprintf("%s#%d", c.Status.PR.Repo, c.Status.PR.Number)
}

// sortRefs sorts refs by repo, then number.
func sortRefs(refs []string) {
	sort.SliceStable(refs, func(i, j int) bool { return refLess(refs[i], refs[j]) })
}

func refLess(a, b string) bool {
	ra, ea := v1alpha1.ParseRef(a)
	rb, eb := v1alpha1.ParseRef(b)
	if ea != nil || eb != nil {
		return a < b
	}
	if ra.FullRepo() != rb.FullRepo() {
		return ra.FullRepo() < rb.FullRepo()
	}
	return ra.Number < rb.Number
}

func isParticipant(participants []string, login string) bool {
	for _, p := range participants {
		if strings.EqualFold(p, login) {
			return true
		}
	}
	return false
}

func optedIn(team *v1alpha1.Team) bool {
	if team == nil {
		return false
	}
	for _, w := range team.Spec.OptIn.Work {
		if strings.EqualFold(w, WorkImplementable) {
			return true
		}
	}
	return false
}

func teamName(team *v1alpha1.Team) string {
	if team == nil {
		return ""
	}
	return team.Name
}

func mt(t time.Time) metav1.Time { return metav1.NewTime(t) }

func mtp(t time.Time) *metav1.Time {
	m := metav1.NewTime(t)
	return &m
}

func md(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func earliest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.Before(*a):
		return b
	default:
		return a
	}
}

func timep(t time.Time) *time.Time { return &t }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// event builds a history row.
func event(at time.Time, typ, source string, data map[string]any) history.Event {
	return history.Event{At: at, Type: typ, Source: source, Data: data}
}
