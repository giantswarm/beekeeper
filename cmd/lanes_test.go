package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A lane whose arrived merge waits behind a seed that has not arrived is
// stalled in lanes and one LANE STALLED line of the watch, folded while it
// lasts.
func TestLanesAndWatchSayAStall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\nlanes: [{name: ap, repositories: [giantswarm/klaus]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	const repo, five = "giantswarm/klaus", "Agent seven"
	now := time.Now()
	err = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			{Repo: repo, PR: 676, Lane: "ap", By: state.Party{Name: "GPU"}, Phase: state.Waiting, Seeded: true, Joined: now, Seen: now},
			{Repo: repo, PR: 671, Lane: "ap", By: state.Party{Name: five}, PID: os.Getpid(), Phase: state.Waiting, Seeded: true,
				Joined: now.Add(time.Second), Seen: now},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: cfg, store: store, now: now.Add(cfg.Merge.StallAfter.Duration + time.Minute), out: &out}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	a.printLanes(a.laneViews(st))
	if want := `stalled for 6m`; !strings.Contains(out.String(), want) ||
		!strings.Contains(out.String(), repo+`#671 ("`+five+`") waits in the gate behind `+repo+`#676 ("GPU", not arrived)`) {
		t.Errorf("lanes does not say the stall (%q):\n%s", want, out.String())
	}

	out.Reset()
	w := &watcher{app: a, last: map[string]time.Time{}}
	w.stalls()
	w.stalls()
	if lines := strings.Count(out.String(), "LANE STALLED ap: stalled for"); lines != 1 {
		t.Errorf("watch said the stall %d times:\n%s", lines, out.String())
	}
}

// gazelleHRs is `kubectl get helmreleases -A -o json` of gazelle's
// agent-platform charts: chartRef sources, so the chart name is only in the
// history, and chart versions with build metadata.
func gazelleHRs(version, ready string) string {
	hr := func(name string) string {
		return `{"metadata": {"namespace": "flux-giantswarm", "name": "` + name + `"},
			"spec": {"chartRef": {"kind": "OCIRepository", "name": "` + name + `"}},
			"status": {"history": [{"chartName": "` + name + `", "chartVersion": "` + version + `+5848dde334c3"}],
				"conditions": [{"type": "Ready", "status": "` + ready + `", "message": "Helm upgrade succeeded"}]}}`
	}
	return `{"items": [` + hr("agent-platform") + `, ` + hr("agent-platform-connectivity") + `]}`
}

// A settling merge whose release rolls only after merge.settleTimeout is
// one LANE STUCK line while it waits and leaves its lane once the lane is
// Ready, with one ENDED line: the watch never stops checking it.
func TestWatchSettlesALaneReadyAfterTheSettleTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\nlanes: [{name: agent-platform, installation: gazelle, repositories: [giantswarm/agent-platform]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{{Repo: "giantswarm/agent-platform", PR: 701, Lane: "agent-platform", Phase: state.Settling,
			Release: "v4.79.0", Roll: []string{"flux-giantswarm/agent-platform"}, Finished: now.Add(-cfg.Merge.SettleTimeout.Duration - 10*time.Minute)}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: cfg, store: store, now: now, out: &out}
	raw, readErr := gazelleHRs("4.78.0", "True"), error(nil)
	w := &watcher{app: a, last: map[string]time.Time{}, readHRs: func(context.Context, config.Lane) ([]merge.HelmRelease, error) {
		if readErr != nil {
			return nil, readErr
		}
		return merge.ParseHelmReleases([]byte(raw))
	}}
	settling := func() bool {
		st, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		return len(st.Merges) == 1
	}

	w.settled(context.Background())
	w.settled(context.Background())
	if !settling() || strings.Count(out.String(), "LANE STUCK agent-platform: giantswarm/agent-platform#701 has not settled 40m after its merge: flux-giantswarm/agent-platform is on 4.78.0, rolling to 4.79.0") != 1 {
		t.Fatalf("a release that has not rolled is not one LANE STUCK line:\n%s", out.String())
	}
	st, _ := store.Read()
	a.printLanes(a.laneViews(st))
	if !strings.Contains(out.String(), "settling giantswarm/agent-platform#701 until v4.79.0 rolls, stuck since") {
		t.Errorf("lanes does not say the settle is stuck:\n%s", out.String())
	}

	out.Reset()
	readErr = errors.New("exec: tsh: not logged in")
	w.settled(context.Background())
	if !settling() {
		t.Fatal("an unreadable installation settled the merge")
	}

	out.Reset()
	raw, readErr = gazelleHRs("4.79.0", "True"), nil
	w.settled(context.Background())
	if settling() {
		t.Fatalf("the lane Ready on 4.79.0+5848dde334c3 past the settle timeout still settles:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ENDED LANE STUCK agent-platform") {
		t.Errorf("the stuck lane's end is not said:\n%s", out.String())
	}
}
