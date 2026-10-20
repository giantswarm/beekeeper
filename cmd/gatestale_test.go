//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
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

// A gate call of a release older than the one that wrote a state it does
// not decode (a field the newer release changed) re-executes its replaced
// binary, and refuses (77) naming both releases only when the new binary
// does not start, the state untouched.
func TestAnOlderReleasesGateNamesTheSkewOfAStateItDoesNotLoad(t *testing.T) {
	stubGitHub(t, github.Open, "")
	a := queueApp(t)
	path := filepath.Join(a.store.Dir(), "state.json")
	newer := `{"writer":{"version":"` + newerRelease + `"},"nextNote":"three"}`
	if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if a.store, err = state.OpenVersion(a.store.Dir(), olderRelease); err != nil {
		t.Fatal(err)
	}
	bin := &fakeBinary{replaced: true, execErr: errBusy}
	was := runningBinary
	runningBinary = func() binary { return bin }
	t.Cleanup(func() { runningBinary = was })
	stderr := capture(t, &os.Stderr)
	err = a.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, 0, false)
	said := stderr()
	if Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v), want %d\n%s", Code(err), err, ExitGateRefused, said)
	}
	if len(bin.execs) != 2 {
		t.Errorf("re-exec attempts %q, want two", bin.execs)
	}
	if !strings.Contains(said, GatePrefix+"refused, o/r#7: this call runs beekeeper v0.1.0, older than the v0.2.0 that wrote the state, which it does not load (") ||
		!strings.Contains(said, "the lane is untouched") {
		t.Errorf("the refusal does not name the skew:\n%s", said)
	}
	if raw, _ := os.ReadFile(path); string(raw) != newer { //nolint:gosec // a test file
		t.Errorf("the state changed: %s", raw)
	}
}

// A merge whose turn came while a newer release wrote the state hands the
// refused save to the wait loop, which re-executes or names the skew,
// instead of refusing it as a state that does not load.
func TestAStaleStartIsTheSkewNotAnUnloadableState(t *testing.T) {
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	newer, err := state.OpenVersion(g.store.Dir(), newerRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Phase, st.Merges[0].Started = state.Waiting, time.Time{}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if g.store, err = state.OpenVersion(g.store.Dir(), olderRelease); err != nil {
		t.Fatal(err)
	}
	_, err = g.start("", nil)
	var stale *state.StaleWriterError
	if !errors.As(err, &stale) || stale.Version != olderRelease || stale.Newer != newerRelease {
		t.Fatalf("start: %v, want the stale writer", err)
	}
	if d := lastEventOf(t, g.app, "merge.refused"); d != "" {
		t.Errorf("refused as it is: %q", d)
	}
}
