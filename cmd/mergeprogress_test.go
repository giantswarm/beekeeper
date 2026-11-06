package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestMergeProgressTracksPhasesAndSilence(t *testing.T) {
	base := filepath.Join(t.TempDir(), gitMerge)
	start := time.Now().UTC()
	p := newMergeProgress(base, []string{timeoutFlag, "9m", "--release-timeout=30m"}, start)
	for i, step := range []struct{ line, phase string }{
		{"poll 1: waiting for checks\n", "CI"},
		{"poll 2: green\n", gitMerge},
		{"merged: squash\nrelease: waiting for the release of abc\n", "release wait: tag"},
		{"tag v1.0.0 at abc: release model binary, CI circleci\n", releasePipelinePhase},
		{"pipeline 123: workflow build running\n", releasePipelinePhase},
		{"expecting binary archive\n", "release wait: assets"},
	} {
		now := start.Add(time.Duration(i+1) * time.Minute)
		p.observe(step.line, now)
		got := readMergeProgress(base)
		if got == nil || got.Phase != step.phase {
			t.Fatalf("step %d: %+v", i, got)
		}
		if i == 4 && !got.Since.Equal(start.Add(4*time.Minute)) {
			t.Fatalf("pipeline poll reset phase age: %+v", got)
		}
	}
	if silentMerge(base, start, p.Last.Add(29*time.Minute), time.Minute) {
		t.Fatal("release declared hung before its timeout")
	}
	if !silentMerge(base, start, p.Last.Add(31*time.Minute), time.Hour) {
		t.Fatal("silent release not detected after its timeout")
	}
}

func TestLanesShowsReleasePipelineAge(t *testing.T) {
	now := time.Now().UTC()
	var out bytes.Buffer
	a := &app{out: &out, now: now}
	a.printLanes([]laneView{{Lane: merge.Lane{Name: "test", Running: &state.Merge{Repo: scratchRepo, PR: 7, Started: now.Add(-time.Hour)}}, Progress: &mergeProgress{Phase: releasePipelinePhase, Since: now.Add(-21 * time.Minute)}}})
	if !strings.Contains(out.String(), "phase release wait: pipeline for 21m") {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), "unheard") || strings.Contains(out.String(), "hung") {
		t.Fatal(out.String())
	}
}

func TestActiveMergeOutputPreventsHungDetection(t *testing.T) {
	base := filepath.Join(t.TempDir(), gitMerge)
	start := time.Now().UTC().Add(-2 * time.Hour)
	p := newMergeProgress(base, nil, start)
	p.observe("release: waiting for the release of abc\n", start.Add(time.Minute))
	now := start.Add(2 * time.Hour)
	p.observe("pipeline 123: workflow build running\n", now)
	if silentMerge(base, start, now, 45*time.Minute) {
		t.Fatal("active CircleCI wait is hung")
	}
	// Children started before this change still get credit for log output.
	if err := os.Remove(base + ".progress"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".log", []byte("pipeline running\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if silentMerge(base, start, now, 45*time.Minute) {
		t.Fatal("active legacy child is hung")
	}
}

func TestMergeChildCapturesProgress(t *testing.T) {
	base := filepath.Join(t.TempDir(), gitMerge)
	out, err := os.Create(filepath.Clean(base + ".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	log, err := os.Create(filepath.Clean(base + ".log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	var current atomic.Pointer[os.Process]
	var stop atomic.Value
	var gone atomic.Bool
	rc := runSpec(base, childSpec{Argv: []string{"sh", "-c", "printf 'release: waiting for the release of abc\\npipeline 123: workflow build running\\n' >&2"}}, out, log, &current, &stop, &gone)
	if rc != 0 {
		t.Fatalf("exit %d", rc)
	}
	p := readMergeProgress(base)
	if p == nil || p.Phase != releasePipelinePhase {
		t.Fatalf("child progress: %+v", p)
	}
	raw, err := os.ReadFile(filepath.Clean(base + ".log"))
	if err != nil || !strings.Contains(string(raw), "pipeline 123") {
		t.Fatalf("progress log %q: %v", raw, err)
	}
}
