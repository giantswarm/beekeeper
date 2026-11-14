package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
)

func TestParseReleaseTarget(t *testing.T) {
	for _, on := range []string{"example/tool@v1.2.3", "example/tool@1.2.3-rc.1+build.2"} {
		repo, version, ok := parseReleaseTarget(on)
		if !ok || repo+"@"+version != on {
			t.Fatalf("parse %q: %q %q %v", on, repo, version, ok)
		}
	}
	for _, on := range []string{litExampleTool12, "example/tool@latest", "example/tool@v1", "example/tool@>=1.2.3", "example/tool@", "tool@v1.2.3", "example/tool@v1.2.3 extra"} {
		if _, _, ok := parseReleaseTarget(on); ok {
			t.Errorf("accepted %q", on)
		}
	}
}

func TestParkReleaseAvailability(t *testing.T) {
	w := parkingWatch(t, nil)
	const target = "example/tool@v1.2.3"
	if err := park(t, w, "--on", target, "release to promote"); err != nil {
		t.Fatal(err)
	}
	ag := agentOf(t, w)
	if ag.Park == nil || ag.Park.On != target || ag.Park.ReviewHead != "" {
		t.Fatalf("park %+v", ag.Park)
	}
	if got := parkedText(ag.Park, watchApp(w.Host).now); !strings.Contains(got, target) {
		t.Fatal(got)
	}
	old := watch.ReadParkRelease
	t.Cleanup(func() { watch.ReadParkRelease = old })
	for _, tc := range []struct {
		name, raw string
		err       error
		want      int
	}{
		{name: "release still building", raw: `{"verdict":"timeout","exitCode":2}`, err: errors.New("exit 2")},
		{name: "unavailable tool", err: errors.New("missing devctl")},
		{name: handoverMalformedMode, raw: `not JSON`},
		{name: "failed CI", raw: `{"verdict":"ci_failed","exitCode":1}`},
		{name: "available", raw: `{"verdict":"available","exitCode":0}`, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			watch.ReadParkRelease = func(_ context.Context, repo, version string) ([]byte, error) {
				calls++
				if repo != "example/tool" || version != "v1.2.3" {
					t.Fatalf("target %s %s", repo, version)
				}
				return []byte(tc.raw), tc.err
			}
			st, _ := watchApp(w.Host).store.Read()
			st.Parks = append(st.Parks, state.Park{By: state.Party{Name: "off roster", Session: "release-session"}, On: target})
			settled := w.SettledParks(context.Background(), st)
			if len(settled) != tc.want || calls != 1 {
				t.Fatalf("settled %+v, calls %d", settled, calls)
			}
			if tc.want > 0 {
				lines, _ := watch.MarkResumable(st, settled, true, watchApp(w.Host).now)
				if len(lines) != 2 || !strings.Contains(lines[0], "is available") {
					t.Fatalf("wake lines %v", lines)
				}
				if again := w.SettledParks(context.Background(), st); len(again) != 0 {
					t.Fatalf("repeated wake: %+v", again)
				}
			}
		})
	}
}

func TestServeReleaseTarget(t *testing.T) {
	a, store := serveApp(t)
	const target = "example/tool@v1.2.3"
	if out, err := serve(a, rosterName, target, "--waits", "release availability"); err != nil || !strings.Contains(out, target) {
		t.Fatalf("serve %q: %v", out, err)
	}
	st, err := store.Read()
	if err != nil || len(st.Records) != 1 || st.Records[0].Issue != target {
		t.Fatalf("records %+v: %v", st.Records, err)
	}
}

func TestReleaseParkWakesOnce(t *testing.T) {
	w := parkingWatch(t, nil)
	watchApp(w.Host).cfg.Agents.AutoResume = true
	woke := stubWakes(t)
	const target = "example/tool@v1.2.3"
	if err := park(t, w, "--on", target, "release to promote"); err != nil {
		t.Fatal(err)
	}
	old := watch.ReadParkRelease
	t.Cleanup(func() { watch.ReadParkRelease = old })
	available := false
	watch.ReadParkRelease = func(context.Context, string, string) ([]byte, error) {
		if available {
			return []byte(`{"verdict":"available","exitCode":0}`), nil
		}
		return []byte(`{"verdict":"timeout","exitCode":2}`), nil
	}
	w.Pending(context.Background(), nil)
	if len(*woke) != 0 {
		t.Fatalf("woke before availability: %v", *woke)
	}
	available = true
	for range 3 {
		w.Pending(context.Background(), nil)
	}
	assertFreshQueue(t, w, *woke, "release "+target+" is available")

}

func TestExecutionParkReleaseTarget(t *testing.T) {
	a, r := executionTestApp(t)
	const target = "example/tool@v1.2.3"
	request := execution.ControlRequest{Ref: r.Ref, ID: "release-park", Sequence: 1, Operation: execution.ControlPark, Data: mustExecutionJSON(t, executionPark{On: target, Waits: "release to promote"})}
	var result execution.ControlResult
	if err := coordinatorTestCall(t, a, r, "control", request, &result, false); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if p := st.Agents[0].Park; p == nil || p.On != target || p.ReviewHead != "" {
		t.Fatalf("park %+v, result %+v", p, result)
	}
}
