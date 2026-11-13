package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/Masterminds/semver/v3"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// verdictUnconfirmed is a merge whose release nobody could confirm.
const (
	VerdictUnconfirmed = "unconfirmed"
	VerdictAvailable   = "available"
)

// StaleMerge is local evidence that a running place has lost its writer.
// Unknown versions are not evidence of staleness.
func StaleMerge(st *state.State, m state.Merge) string {
	if m.Phase != state.Running {
		return ""
	}
	if !proc.Alive(m.PID) {
		return fmt.Sprintf("gate pid %d is gone", m.PID)
	}
	if st.Writer != nil {
		version := m.Version
		// Places written before Version was recorded can still identify an
		// executable that self-update replaced.
		if version == "" {
			if _, replaced := replacedBinary(m.PID); replaced {
				version = binaryVersion(context.Background(), fmt.Sprintf("/proc/%d/exe", m.PID))
			}
		}
		own, err := semver.NewVersion(version)
		newer, e := semver.NewVersion(st.Writer.Version)
		if err == nil && e == nil && own.LessThan(newer) {
			return fmt.Sprintf("runs %s, older than state writer %s", version, st.Writer.Version)
		}
	}
	return ""
}

// Takeover checks GitHub before trusting a duplicate's running place. Reads
// happen outside the lock; the exact owner and phase are checked again when
// claiming it so concurrent retries cannot both take it over.
func (g *GateRun) Takeover() (bool, error) {
	st, err := g.Host.store.Read()
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(st.Merges, func(m state.Merge) bool {
		return m.Repo == g.Repo && m.PR == g.Pr &&
			(m.Phase == state.Running && m.PID != g.Pid ||
				g.Pr != 0 && (m.Phase == state.Waiting || g.Placed && m.Phase == state.Settling))
	})
	if i < 0 {
		return false, nil
	}
	old := st.Merges[i]
	if old.Phase == state.Waiting && old.PID == g.Pid && g.Host.now.Sub(old.Checked) < outsideCheck {
		return false, nil
	}
	why := StaleMerge(st, old)
	var p github.Pull
	var readErr error
	if g.Pr != 0 {
		p, readErr = PullState(g.Ctx, g.Repo, g.Pr)
	}
	if p.State == github.Merged {
		why = "pull request merged on GitHub"
	}
	if why == "" {
		if readErr == nil && old.Phase == state.Waiting {
			err = g.Host.store.Update(func(st *state.State) ([]state.Event, error) {
				if i := g.Mine(st, state.Waiting); i >= 0 && st.Merges[i].PID == old.PID {
					st.Merges[i].Checked = g.Host.now.UTC()
				}
				return nil, nil
			})
		}
		return false, err
	}
	// A failed GitHub read cannot safely authorize another merge.
	if readErr != nil {
		return false, g.RefuseWith(ExitGateUnread, "%s has a stale place (%s), but GitHub does not answer (%v): retry once it answers", g.Key(), why, readErr)
	}
	if p.State != github.Merged && proc.Alive(old.Child) {
		base := MergeBase(g.Host.store.Dir(), old.Repo, old.PR)
		if readPID(base) != old.Child {
			return false, g.Refuse("%s has a stale gate but its merge-child cannot be verified", g.Key())
		}
		if err := endChild(old.Child); err != nil || !WaitGone(old.Child, StopWait) {
			return false, g.Refuse("%s has a stale gate but its merge-child did not stop", g.Key())
		}
		// It may have merged while stopping. Ask again before starting any
		// replacement command.
		if g.Pr != 0 {
			p, readErr = PullState(g.Ctx, g.Repo, g.Pr)
			if readErr != nil {
				return false, g.RefuseWith(ExitGateUnread, "%s: GitHub does not answer after stopping the stale run (%v)", g.Key(), readErr)
			}
		}
	}
	claimed := false
	err = g.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Merges, func(m state.Merge) bool {
			return m.Key() == old.Key() && m.PID == old.PID && m.Child == old.Child && m.Phase == old.Phase &&
				m.Version == old.Version && m.Started.Equal(old.Started) && m.Joined.Equal(old.Joined)
		})
		if i < 0 {
			return nil, nil
		}
		// A run that acquired another child while GitHub was read retains it.
		if p.State != github.Merged && proc.Alive(st.Merges[i].Child) {
			return nil, fmt.Errorf("%s has a stale gate but its merge-child (pid %d) still runs; stop it with beekeeper gate stop before retrying", g.Key(), old.Child)
		}
		m := &st.Merges[i]
		m.PID, m.Child, m.By, m.Version = g.Pid, 0, g.Me, project.Version()
		m.Run, m.Output = g.queuedRun(), g.Output
		if p.State == github.Merged {
			m.Phase, m.Finished, m.Exit = state.Settling, merge.Judged(p).SettlesFrom(g.Host.now.UTC()), 0
			m.Release, m.ReleaseFailed, m.Releasing = "", false, true
		} else {
			m.Phase, m.Started, m.Seen = state.Waiting, time.Time{}, g.Host.now.UTC()
		}
		claimed, g.Placed = true, true
		return []state.Event{event(g.Me, "merge.takeover", "%s: old pid %d, %s", g.Key(), old.PID, why)}, nil
	})
	if err != nil || !claimed || p.State != github.Merged {
		return false, err
	}
	return true, g.recordTakenMerge(p, old.HandCut)
}

// recordTakenMerge awaits the existing merge's release, never another merge.
// The place remains owned by this call until the release verdict is saved.
func (g *GateRun) recordTakenMerge(p github.Pull, handCut string) error {
	out := merge.Judged(p)
	rc := 0
	if g.RunsDevctl() && handCut == "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(g.Ctx), ReleasePRWait)
		released, _, ok := merge.ParseReleaseWait(ReleaseWait(ctx, g.Repo, "--pr", strconv.Itoa(g.Pr)))
		cancel()
		if ok {
			out = released
			out.MergedAt = p.MergedAt
		}
		if out.ReleaseFailed {
			rc = DevctlReleaseFailed
		} else if out.Unconfirmed {
			rc = DevctlUnconfirmed
		}
	} else {
		out.NoRelease, out.Unconfirmed = true, false
	}
	err := g.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		i := g.Mine(st, state.Settling)
		if i < 0 {
			return nil, fmt.Errorf("%s lost its takeover place before its outcome was recorded", g.Key())
		}
		st.Merges[i].Releasing = false
		return recordRun(st, i, g.Lane, g.Me, RunOutcome{Out: out, Rc: rc}, clk.Now().UTC(), ", recovered from GitHub"), nil
	})
	if err != nil {
		return err
	}
	// Keep stdout machine readable, as for a normal gated merge.
	verdict := VerdictAvailable
	switch {
	case out.ReleaseFailed:
		verdict = "ci_failed"
	case out.Unconfirmed:
		verdict = VerdictUnconfirmed
	case out.NoRelease:
		verdict = "no_release"
	}
	type releaseDoc struct {
		Tag     string `json:"tag"`
		Verdict string `json:"verdict"`
	}
	doc := struct {
		MergeCommitSHA string     `json:"mergeCommitSha"`
		Verdict        string     `json:"verdict"`
		ExitCode       int        `json:"exitCode"`
		Release        releaseDoc `json:"release"`
	}{p.MergeCommit.OID, "merged", rc, releaseDoc{out.Release, verdict}}
	if err := json.NewEncoder(os.Stdout).Encode(doc); err != nil {
		return err
	}
	GateLine("%s merged, recovered outcome: release %s", g.Key(), out.Release)
	return ExitCode(rc)
}
