package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// watchHarness models harness discovery while a persistent runner is alive,
// including the interval before its first transcript and between turns.
type watchHarness struct {
	Harness
	session *claude.Session
}

func (h *watchHarness) Discover(*config.Config, *proc.Table, time.Time) []*claude.Session {
	if h.session == nil {
		return nil
	}
	return []*claude.Session{h.session}
}

func TestWatchTracksEveryHarnessRunner(t *testing.T) {
	const stopped = "AGENTS STOPPED"
	const ended = "SESSION ENDED"
	for _, name := range []string{codex.Harness, omp.Harness, agy.Harness} {
		t.Run(name, func(t *testing.T) {
			h, _ := harnessByName(name)
			fake := &watchHarness{Harness: h, session: &claude.Session{ID: "runner", HostID: h.HostPrefix() + "runner", Name: ownerName, Harness: name, PID: 123456789}}
			original := harnesses
			harnesses = []Harness{fake}
			t.Cleanup(func() { harnesses = original })
			dir := t.TempDir()
			w, _, out := notifyingWatch(t, dir, false)
			party := fake.session.Party()
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: party, Task: "finish task", Harness: name}}
				st.Records = []state.Record{{Session: party, Issue: "https://github.com/example/project/issues/1"}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			table := &proc.Table{ByPID: map[int]*proc.Process{}}
			poll := func() { w.pollSessions(context.Background(), relayNow, table) }
			poll()
			poll()
			// A replacement watch must rediscover the same still-running worker.
			restarted, _, restartOut := notifyingWatch(t, dir, false)
			restarted.pollSessions(context.Background(), relayNow, table)
			if got := restartOut.String(); strings.Contains(got, stopped) || strings.Contains(got, ended) {
				t.Fatalf("replacement watch reported a live runner stopped:\n%s", got)
			}
			if got := out.String(); strings.Contains(got, stopped) || strings.Contains(got, ended) {
				t.Fatalf("live runner reported stopped:\n%s", got)
			}
			fake.session = nil
			poll()
			poll()
			got := out.String()
			if strings.Count(got, stopped) != 1 || strings.Count(got, ended) != 1 {
				t.Fatalf("runner end not reported once:\n%s", got)
			}
			if strings.Contains(got, "claude://") || !strings.Contains(got, `beekeeper agents wake "worker"`) {
				t.Fatalf("wrong harness resume hint:\n%s", got)
			}
		})
	}
}

func TestHarnessResumeHints(t *testing.T) {
	for _, name := range []string{codex.Harness, omp.Harness, agy.Harness} {
		for _, ag := range []state.Agent{
			{Party: state.Party{Name: ownerName, HostSession: name + "_runner"}},
			{Party: state.Party{Name: ownerName}, Harness: name},
		} {
			got := resumeHint(ag)
			if strings.Contains(got, "claude") || !strings.Contains(got, `beekeeper agents wake "worker"`) {
				t.Errorf("%+v: %s", ag, got)
			}
		}
	}
}
