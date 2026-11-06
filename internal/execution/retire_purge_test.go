package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// retiredClean retires the fixture's launched execution, optionally letting
// prepare leave something behind first, and returns the moved-aside root.
func retiredClean(t *testing.T, f fixture, prepare func(Handle)) (Retirement, string) {
	t.Helper()
	h := launched(t, f)
	if prepare != nil {
		prepare(h)
	}
	f.launcher.states[h.Unit] = unitInactive
	x, err := f.s.Retire(context.Background(), f.r.Ref, retireRequest(RetireTriggerReport))
	if err != nil || x.Stage != RetireRetired {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	return x, retiredRoot(f)
}

func pastRetention(f fixture, x Retirement) {
	f.s.Now = func() time.Time { return x.RetainUntil.Add(time.Second) }
}

func journal(t *testing.T, f fixture) Journal {
	t.Helper()
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// A clean execution's resources stay through retention and go with the next
// coordinator retirement after it, the worker's scratch and caches included;
// the journal stays as evidence.
func TestRetirePurgesDisposableResourcesAfterRetention(t *testing.T) {
	f := testFixture(t)
	source := f.s.sourcePath(f.r.Ref)
	x, root := retiredClean(t, f, func(h Handle) {
		writeFixture(t, filepath.Join(h.Workspace, scratchDir, "note"), "scratch\n")
		writeFixture(t, filepath.Join(filepath.Dir(h.Workspace), stateDir, "go-toolchain", "bin"), "toolchain\n")
		writeFixture(t, source, "owned source bundle\n")
	})
	if !journal(t, f).Disposable {
		t.Fatalf("clean execution is not disposable: %+v", x)
	}
	aside := filepath.Join(f.s.Root, retiredResources, retiredLabel(f.r.Ref))
	f.s.purgeRetired(context.Background())
	if !exists(t, root) || !exists(t, aside) {
		t.Fatal("purged within retention")
	}
	pastRetention(f, x)
	again, err := f.s.Retire(context.Background(), f.r.Ref, retireRequest(RetireTriggerReport))
	if err != nil || again.ID != x.ID {
		t.Fatalf("rerun = %+v, %v", again, err)
	}
	if exists(t, root) || exists(t, aside) {
		t.Fatal("disposable resources outlived retention")
	}
	if j := journal(t, f); j.PurgedAt.IsZero() || j.Retirement.LastError != "" {
		t.Fatalf("journal evidence: %+v", j.Retirement)
	}
}

// Neither stopping nor inspecting removes anything, retention passed or not:
// only a coordinator's retirement does.
func TestRetireStopAndInspectNeverPurge(t *testing.T) {
	f := testFixture(t)
	x, root := retiredClean(t, f, nil)
	pastRetention(f, x)
	if _, err := f.s.Inspect(context.Background(), f.r.Ref); err != nil {
		t.Fatal(err)
	}
	if !exists(t, root) {
		t.Fatal("inspection purged a retired execution")
	}
}

// Work a bundle holds is never expired, whatever its retention says.
func TestRetirePurgeKeepsRecovery(t *testing.T) {
	f := testFixture(t)
	x, root := retiredClean(t, f, func(h Handle) { leaveWork(t, f, h) })
	if x.Recovery == nil || journal(t, f).Disposable {
		t.Fatalf("retirement = %+v", x)
	}
	pastRetention(f, x)
	f.s.purgeRetired(context.Background())
	if !exists(t, filepath.Join(root, "workspace", retireFixtureTracked)) {
		t.Fatal("purged recoverable work")
	}
}

// Ignored files are in no bundle: the moved-aside tree is their only copy.
func TestRetirePurgeKeepsIgnoredFiles(t *testing.T) {
	f := testFixture(t)
	x, root := retiredClean(t, f, func(h Handle) {
		repository := filepath.Join(filepath.Dir(h.Workspace), "repository.git")
		writeFixture(t, filepath.Join(repository, "info", "exclude"), "kept.env\n"+scratchDir+"/\n")
		writeFixture(t, filepath.Join(h.Workspace, "kept.env"), "ignored work\n")
	})
	if x.Recovery != nil || journal(t, f).Disposable {
		t.Fatalf("retirement = %+v", x)
	}
	pastRetention(f, x)
	f.s.purgeRetired(context.Background())
	if !exists(t, filepath.Join(root, "workspace", "kept.env")) {
		t.Fatal("purged an ignored file")
	}
}

// A retirement recorded before the proof existed proves nothing and stays.
func TestRetirePurgeKeepsAnUnprovenRetirement(t *testing.T) {
	f := testFixture(t)
	x, root := retiredClean(t, f, nil)
	if err := f.s.update(context.Background(), f.r.Ref, func(j *Journal) error {
		j.Disposable = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pastRetention(f, x)
	f.s.purgeRetired(context.Background())
	if !exists(t, root) {
		t.Fatal("purged an unproven retirement")
	}
}

// A retired root replaced by a symlink is left alone, its target too, and the
// refusal is recorded; a later retirement tries again.
func TestRetirePurgeRefusesASymlinkedAside(t *testing.T) {
	f := testFixture(t)
	x, root := retiredClean(t, f, nil)
	elsewhere := t.TempDir()
	writeFixture(t, filepath.Join(elsewhere, "kept"), "not owned\n")
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatal(err)
	}
	pastRetention(f, x)
	f.s.purgeRetired(context.Background())
	if !exists(t, root) || !exists(t, filepath.Join(elsewhere, "kept")) {
		t.Fatal("followed or removed a symlinked aside")
	}
	if j := journal(t, f); j.Retirement.LastError == "" || !j.PurgedAt.IsZero() {
		t.Fatalf("refusal not recorded: %+v", j.Retirement)
	}
}
