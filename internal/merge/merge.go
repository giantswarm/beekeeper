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
	// Release is the tag the merge released, empty when none or unknown.
	Release string
	// NoRelease is true when the merge warranted no release: nothing rolls.
	NoRelease bool
}

// ParseDocument reads devctl pr merge's JSON document; ok is false when it
// is none.
func ParseDocument(raw []byte) (Outcome, bool) {
	var doc struct {
		MergeCommitSha string `json:"mergeCommitSha"`
		Release        *struct {
			Verdict string `json:"verdict"`
			Result  *struct {
				Tag string `json:"tag"`
			} `json:"result"`
		} `json:"release"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return Outcome{}, false
	}
	o := Outcome{Merged: doc.MergeCommitSha != ""}
	if doc.Release != nil {
		o.NoRelease = doc.Release.Verdict == "no_release"
		if doc.Release.Result != nil {
			o.Release = doc.Release.Result.Tag
		}
	}
	return o, true
}

// Blocking is the active hold that stops a merge into repo in lane: the
// repository's, the lane's, github's or one on all merges.
func Blocking(st *state.State, now time.Time, repo, lane string) (state.Hold, bool) {
	for _, h := range st.Holds {
		if !h.Active(now) {
			continue
		}
		switch h.Target {
		case repo, LanePrefix + lane, "github":
			return h, true
		case AllMerges:
			if !strings.EqualFold(h.Except, repo) {
				return h, true
			}
		}
	}
	return state.Hold{}, false
}

// Prune drops the waiting merges whose run ended more than ttl ago (seedTTL
// for a seeded place) and turns
// a running merge whose run is gone into a settling one: whether it merged
// is unknown, so the lane settles by the settle rule.
func Prune(st *state.State, now time.Time, ttl, seedTTL time.Duration, alive func(pid int) bool) {
	st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
		keep := ttl
		if m.Seeded {
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
	Name     string        `json:"name"`
	Running  *state.Merge  `json:"running,omitempty"`
	Settling *state.Merge  `json:"settling,omitempty"`
	Waiting  []state.Merge `json:"waiting"`
}

// Queue returns the lane's running and settling merges and the waiting
// ones in turn order.
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
			q.Settling = m
		default:
			q.Waiting = append(q.Waiting, *m)
		}
	}
	slices.SortStableFunc(q.Waiting, func(a, b state.Merge) int { return a.Joined.Compare(b.Joined) })
	return q
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
	if settling != nil {
		if settling.Release == "" {
			if until := settling.Finished.Add(settle); now.Before(until) {
				return false, fmt.Sprintf("%s's release is unknown: the lane settles until %s", settling.Key(), until.Local().Format("15:04"))
			}
		}
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
