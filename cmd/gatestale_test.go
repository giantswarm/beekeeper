//go:build unix

package cmd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The releases of the tests' stale gates: the one that wrote the state and
// the older one a gate call runs.
const (
	newerRelease = "v0.2.0"
	olderRelease = "v0.1.0"
)

// A gate call of a release older than the one that wrote the state (an
// install while it waited, its binary not replaced at its path) is refused
// before it touches the lane: the lane and the writer stay as the newer
// release left them, and the refusal names both versions.
func TestAnOlderReleasesGateIsRefusedBeforeTheLane(t *testing.T) {
	stubGitHub(t, github.Open, "")
	waiting := state.Merge{Repo: scratchRepo, PR: 3, Lane: scratchRepo, Phase: state.Waiting, By: state.Party{Session: "s2", Name: agentTwo},
		PID: os.Getpid(), Joined: time.Now().UTC(), Seen: time.Now().UTC()}
	a := queueApp(t, waiting)
	newer, err := state.OpenVersion(a.store.Dir(), newerRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.Update(func(*state.State) ([]state.Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if a.store, err = state.OpenVersion(a.store.Dir(), olderRelease); err != nil {
		t.Fatal(err)
	}
	err = a.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, 0, false)
	if Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.HasPrefix(d, "o/r#7: this call runs beekeeper v0.1.0, older than the v0.2.0 that wrote the state") || !strings.Contains(d, "the lane is untouched") {
		t.Errorf("refused event %q, want both versions named", d)
	}
	st, err := newer.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Writer == nil || st.Writer.Version != newerRelease || len(st.Merges) != 1 || st.Merges[0].PR != 3 || st.Merges[0].Phase != state.Waiting {
		t.Errorf("writer %+v, merges %+v: the older release's gate touched the state", st.Writer, st.Merges)
	}
	if d := lastEventOf(t, a, state.VerbStaleWriter); !strings.Contains(d, "its save is refused") {
		t.Errorf("stale-writer event %q", d)
	}
}
