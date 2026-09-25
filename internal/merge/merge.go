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

// Outcome is what devctl's document says about the merge.
type Outcome struct {
	// Merged is true when the document names a merge commit.
	Merged bool
	// Release is the tag the merge released and devctl confirmed pullable,
	// empty when none or unknown.
	Release string
	// NoRelease is true when the merge warranted no release: nothing rolls.
	NoRelease bool
}

// ParseDocument reads devctl pr merge's JSON document (docs/pr-merge.md in
// giantswarm/devctl): its release object carries the verdict and the tag side
// by side. The tag counts only with the verdict available, a release devctl
// confirmed pullable; any other tag is unknown and the lane settles by the
// settle rule. ok is false when it is none.
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
		}
	}
	return o, true
}

// Blocking is the active hold that stops a merge of repo#pr in lane: the
// repository's, the lane's, github's or one on all merges, unless the hold
// lets that repository or pull request through, else an upgrade running on
// the lane's installation.
func Blocking(st *state.State, now time.Time, repo string, pr int, lane config.Lane) (state.Hold, bool) {
	for _, h := range st.Holds {
		if !h.Active(now) || h.Excepts(repo, pr) {
			continue
		}
		switch h.Target {
		case repo, LanePrefix + lane.Name, "github", AllMerges:
			return h, true
		}
	}
	return upgrade.Held(st, lane.Installation, now)
}

// Prune drops the waiting merges whose run ended more than ttl ago (seedTTL
// for a seeded place and a failed attempt's) and turns
// a running merge whose run is gone into a settling one: whether it merged
// is unknown, so the lane settles by the settle rule.
func Prune(st *state.State, now time.Time, ttl, seedTTL time.Duration, alive func(pid int) bool) {
	st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
		keep := ttl
		if m.Seeded || m.Retrying() {
			keep = seedTTL
		}
		return m.Phase == state.Waiting && !alive(m.PID) && now.Sub(m.Seen) > keep
	})
	for i, m := range st.Merges {
		if m.Phase == state.Running && !alive(m.PID) {
			st.Merges[i].Phase, st.Merges[i].Finished, st.Merges[i].Exit = state.Settling, now, -1
			st.Merges[i].Release, st.Merges[i].Roll = "", nil
		}
	}
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
	slices.SortStableFunc(q.Waiting, func(a, b state.Merge) int {
		if a.Outside != b.Outside {
			if a.Outside {
				return -1
			}
			return 1
		}
		return a.Joined.Compare(b.Joined)
	})
	return q
}

// ExitRefused is devctl's refusal (another human's pull request, a
// repository with agentMerge: false): final, no retry follows it.
const ExitRefused = 5

// Failed records a run that ended with exit code rc and nothing merged. The
// merge goes back to waiting at its place, so its session's retry of the same
// pull request runs before the merges that joined behind it; a refusal leaves
// the lane. It reports whether the place is kept.
func Failed(m *state.Merge, rc int, at time.Time) bool {
	if rc == ExitRefused {
		return false
	}
	m.Phase, m.Finished, m.Seen, m.Exit, m.Release, m.Roll = state.Waiting, at.UTC(), at.UTC(), rc, "", nil
	return true
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
	m.Release, m.Roll, m.Checked = "", nil, time.Time{}
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
						Chart string `json:"chart"`
					} `json:"spec"`
				} `json:"chart"`
			} `json:"spec"`
			Status struct {
				History []struct {
					ChartName    string `json:"chartName"`
					ChartVersion string `json:"chartVersion"`
				} `json:"history"`
				Conditions []struct {
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
			Message: "no Ready condition yet"}
		if len(it.Status.History) > 0 {
			hr.Chart, hr.Version = it.Status.History[0].ChartName, bare(it.Status.History[0].ChartVersion)
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				hr.Ready, hr.Message = c.Status == "True", c.Message
			}
		}
		out = append(out, hr)
	}
	return out, nil
}

// bare is a version without a leading v and without build metadata: a tag
// v1.2.3 and a chart version 1.2.3+1c161d9d name the same release.
func bare(v string) string {
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
// that release; one whose release is unknown (merged without a confirmed
// release, or its run lost) has settled once settle has passed since it
// ended. why says what the lane waits for.
func Ready(lane config.Lane, hrs []HelmRelease, settling *state.Merge, now time.Time, settle time.Duration) (bool, string) {
	mine := laneReleases(lane, hrs)
	switch {
	case settling == nil:
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
			case mine[i].Version != bare(settling.Release):
				return false, fmt.Sprintf("%s is on %s, rolling to %s", key, mine[i].Version, bare(settling.Release))
			}
		}
	}
	for _, hr := range mine {
		if !hr.Ready {
			return false, fmt.Sprintf("%s is not Ready: %s", hr.Key, strings.TrimSpace(hr.Message))
		}
	}
	return true, ""
}
