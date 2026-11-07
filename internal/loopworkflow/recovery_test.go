//go:build integration

package loopworkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	goodRelease = "v1.1.0-rc.1"
	nextRelease = "v1.1.0-rc.2"
)

func versioned(t *testing.T, version, overlay string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "beekeeper")
	args := []string{"build", "-o", bin, "-ldflags", "-X github.com/giantswarm/beekeeper/pkg/project.version=" + version}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	c := exec.CommandContext(ctx, "go", append(args, ".")...)
	c.Dir = root
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, out)
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// parserOverlay is the historical defect, the base dropped by the pull parser.
func parserOverlay(t *testing.T) string {
	t.Helper()
	source, err := filepath.Abs("../../cmd/supervisorloop.go")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	const before = "Base: j.Base,"
	if strings.Count(string(original), before) != 1 {
		t.Fatal("parser mutation no longer targets exactly one assignment")
	}
	dir := t.TempDir()
	replacement := filepath.Join(dir, "supervisorloop.go")
	if err = os.WriteFile(replacement, []byte(strings.Replace(string(original), before, `Base: "",`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "overlay.json")
	if err = os.WriteFile(path, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hexSum(raw []byte) string {
	d := sha256.Sum256(raw)
	return hex.EncodeToString(d[:])
}

// desk is an installation in probation: the candidate at target, the good
// release retained beside it, and a state with work in it.
type desk struct {
	dir, target, config, good string
	env                       []string
	store                     *state.FileStore
}

func newDesk(t *testing.T, good, candidate []byte) *desk {
	t.Helper()
	dir := t.TempDir()
	d := &desk{dir: dir, target: filepath.Join(dir, "install", "beekeeper"), config: filepath.Join(dir, "config.yaml")}
	recovery := d.target + ".update"
	for _, path := range []string{filepath.Dir(d.target), recovery, filepath.Join(dir, "path")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	d.good = filepath.Join(recovery, hexSum(good))
	for path, raw := range map[string][]byte{d.target: candidate, d.good: good, filepath.Join(recovery, hexSum(candidate)): candidate} {
		if err := os.WriteFile(path, raw, 0700); err != nil { //nolint:gosec // executables
			t.Fatal(err)
		}
	}
	j := update.Journal{
		Schema: 1, Displaced: update.Binary{Version: goodRelease, Digest: hexSum(good)}, Previous: &update.Binary{Version: goodRelease, Digest: hexSum(good)},
		Candidate: update.Binary{Version: nextRelease, Digest: hexSum(candidate)}, InstalledAt: time.Now().UTC(), Probation: "pending", Watched: true,
	}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recovery, "journal.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.WriteFile(d.config, []byte("stateDir: "+stateDir+"\nidentity:\n  host: fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if d.store, err = state.Open(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := d.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{{ID: 1, Text: "work in flight"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGit, filepath.Join(dir, "path", "git")); err != nil {
		t.Fatal(err)
	}
	// No service manager, bus or real configuration: only git is on the PATH.
	d.env = []string{"HOME=" + filepath.Join(dir, "home"), "XDG_STATE_HOME=" + filepath.Join(dir, "home"), "PATH=" + filepath.Join(dir, "path"), "LANG=C.UTF-8", "TZ=UTC"}
	return d
}

// controller is the retained good release running as the recovery controller,
// a tick a simulated minute.
func (d *desk) controller(ctx context.Context) *exec.Cmd {
	c := exec.CommandContext(ctx, d.good, "--config", d.config, "self-update", "--recovery-controller", "--recovery-target", d.target, "--recovery-every", "50ms", "--recovery-step", "1m") //nolint:gosec // the fixture's binary
	c.Env, c.Dir = d.env, d.dir
	return c
}

func (d *desk) journal(t *testing.T) *update.Journal {
	t.Helper()
	j, err := update.Inspect(d.target)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// TestRecoveryRestoresABrokenCandidate is the post-canary defect: a candidate
// that passed everything else cannot merge on the desk. A controller of the
// good release, killed once mid-way, restores it from the journal alone, with
// no person and no backlog, and the desk merges again.
func TestRecoveryRestoresABrokenCandidate(t *testing.T) {
	good, broken := versioned(t, goodRelease, ""), versioned(t, nextRelease, parserOverlay(t))
	d := newDesk(t, good, broken)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()

	first := d.controller(ctx)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	for {
		if j := d.journal(t); j.Observed != nil && j.Observed.ProbeFailures > 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("no probe failed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	if got, err := update.Digest(d.target); err != nil || got == hexSum(good) {
		t.Fatal("the crash exercise came too late: already restored")
	}

	// The candidate's death changes nothing: the controller runs without it.
	second := d.controller(ctx)
	if out, err := second.CombinedOutput(); err != nil {
		t.Fatalf("controller: %v\n%s", err, out)
	}
	if got, err := update.Digest(d.target); err != nil || got != hexSum(good) {
		t.Fatalf("installed %s, %v", got, err)
	}
	j := d.journal(t)
	if !j.RolledBack() || j.Quarantine[hexSum(broken)] == "" || j.Observed == nil || !j.Observed.Notified {
		t.Fatalf("%+v", j)
	}
	if j.Observed.Elapsed >= update.ProbationWindow {
		t.Fatalf("restored after %s", j.Observed.Elapsed)
	}
	st, err := d.store.Read()
	if err != nil || len(st.Notes) != 1 {
		t.Fatalf("work lost: %+v, %v", st, err)
	}
	evs, err := d.store.Events(0, func(e state.Event) bool { return strings.Contains(e.Detail, "RELEASE ROLLED BACK") })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, nextRelease) || !strings.Contains(evs[0].Detail, goodRelease) {
		t.Fatalf("the person's line: %+v, %v", evs, err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := Probe(ctx, d.target, self, t.TempDir()); err != nil {
		t.Fatalf("the restored desk does not merge: %v", err)
	}
	if err := Probe(ctx, filepath.Join(d.target+".update", hexSum(broken)), self, t.TempDir()); err == nil {
		t.Fatal("the control: the probe passes the broken candidate")
	} else {
		fmt.Fprintln(os.Stderr, "broken candidate:", err)
	}
}

// TestRecoveryPromotesAHealthyCandidate: no backlog, one isolated workflow, and
// a probation that ends with the candidate as the last known good.
func TestRecoveryPromotesAHealthyCandidate(t *testing.T) {
	good, next := versioned(t, goodRelease, ""), versioned(t, nextRelease, "")
	d := newDesk(t, good, next)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	if out, err := d.controller(ctx).CombinedOutput(); err != nil {
		t.Fatalf("controller: %v\n%s", err, out)
	}
	if got, err := update.Digest(d.target); err != nil || got != hexSum(next) {
		t.Fatalf("installed %s, %v", got, err)
	}
	if j := d.journal(t); !j.Settled() || j.RolledBack() || j.Observed == nil || !j.Observed.ProbePassed {
		t.Fatalf("%+v", j)
	}
}

// A retained controller includes its own workflow adapter. Testing two builds
// of today's source cannot establish compatibility with that older adapter.
func TestRetainedRecoveryController(t *testing.T) {
	path := os.Getenv("BEEKEEPER_TEST_RETAINED_CONTROLLER")
	if path == "" {
		t.Skip("set BEEKEEPER_TEST_RETAINED_CONTROLLER to the retained release binary")
	}
	good, err := os.ReadFile(path) //nolint:gosec // explicit integration-test release executable
	if err != nil {
		t.Fatal(err)
	}
	var next []byte
	candidate := os.Getenv("BEEKEEPER_TEST_RECOVERY_CANDIDATE")
	if candidate != "" {
		next, err = os.ReadFile(candidate) //nolint:gosec // explicit integration-test release executable
		if err != nil {
			t.Fatal(err)
		}
	} else {
		next = versioned(t, nextRelease, "")
	}
	d := newDesk(t, good, next)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	identity := func(binary string) string {
		c := exec.CommandContext(ctx, binary, "version") //nolint:gosec // supplied integration-test release
		c.Env, c.Dir = d.env, d.dir
		out, err := c.Output()
		if err != nil || len(strings.Fields(string(out))) < 2 {
			t.Fatalf("binary identity: %v: %s", err, out)
		}
		return strings.Fields(string(out))[1]
	}
	j := d.journal(t)
	j.Previous.Version, j.Displaced.Version = identity(d.good), identity(d.good)
	j.Candidate.Version = identity(d.target)
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.target+".update", "journal.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	failed := os.Getenv("BEEKEEPER_TEST_RECOVERED_FROM")
	if failed == "" {
		failed = j.Previous.Version
	}
	if err := d.store.RecoverVersion(j.Previous.Version, failed, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	out, err := d.controller(ctx).CombinedOutput()
	if err != nil {
		t.Fatalf("retained controller: %v\n%s", err, out)
	}
	j = d.journal(t)
	writer, err := state.OpenVersion(filepath.Join(d.dir, "state"), j.Candidate.Version)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := writer.Update(func(*state.State) ([]state.Event, error) { return nil, nil })
	var fence map[string]any
	fenced, fenceErr := d.store.ReadFile("recovery-writer.json", &fence)
	if j.RolledBack() && (!fenced || fenceErr != nil || !errors.Is(writeErr, state.ErrRecoveryWriterFenced)) {
		t.Fatalf("rollback failed to fence candidate writes: fence=%v, %v, write=%v\n%s", fenced, fenceErr, writeErr, out)
	}
	if !j.Settled() || j.RolledBack() || j.Observed == nil || !j.Observed.ProbePassed {
		t.Fatalf("retained controller did not promote candidate: %+v\n%s", j, out)
	}
	if fenced || fenceErr != nil || writeErr != nil {
		t.Fatalf("normal probation did not clear writer fence: fence=%v, %v, write=%v", fenced, fenceErr, writeErr)
	}
}
