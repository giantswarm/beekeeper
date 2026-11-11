//go:build integration

package loopworkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	goodRelease = "v1.1.0-rc.1"
	nextRelease = "v1.1.0-rc.2"
)

var (
	versionedMu    sync.Mutex
	versionedCache = map[string][]byte{}
)

func versioned(t *testing.T, version, overlay string) []byte {
	t.Helper()
	cacheKey := fmt.Sprintf("%s|overlay=%t", version, overlay != "")
	versionedMu.Lock()
	if cached, ok := versionedCache[cacheKey]; ok {
		versionedMu.Unlock()
		return cached
	}
	versionedMu.Unlock()

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
	versionedMu.Lock()
	versionedCache[cacheKey] = raw
	versionedMu.Unlock()
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
	d.env = []string{"HOME=" + filepath.Join(dir, "home"), "XDG_STATE_HOME=" + filepath.Join(dir, "home"), "PATH=" + filepath.Join(dir, "path"), "LANG=C.UTF-8", "TZ=UTC", envLoadavg}
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
	writeProofArtifact(t, "recovery-proof.json", map[string]any{
		"good_version":        goodRelease,
		"good_sha256":         hexSum(good),
		"broken_version":      nextRelease,
		"broken_sha256":       hexSum(broken),
		"rolled_back":         j.RolledBack(),
		"quarantine_reason":   j.Quarantine[hexSum(broken)],
		"elapsed":             j.Observed.Elapsed.String(),
		"notification_logged": evs[0].Detail,
		"post_rollback_probe": "passed",
	})
}

// TestCanaryContainmentBlocksCoordinatorAndHostUpdate proves that a good release
// candidate passes the full staged canary (worker start, branch push, PR park,
// independent exact-head review, merge validation, and observed merge commit),
// while a candidate with a parser or merge-validation regression fails the
// staged canary, produces failed evidence that blocks coordinator and
// execution-host update routes while preserving the installed executable bytes,
// and that recorded fixture evidence cannot grant release eligibility.
func TestCanaryContainmentBlocksCoordinatorAndHostUpdate(t *testing.T) {
	// This regression exercises future provisioned enforcement. The temporary
	// default-off policy is covered across all install routes in update's tests.
	t.Cleanup(update.SetSandboxProofRequiredForTest())
	good, broken := versioned(t, goodRelease, ""), versioned(t, nextRelease, parserOverlay(t))
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	dir := t.TempDir()
	goodPath := filepath.Join(dir, "beekeeper-good")
	brokenPath := filepath.Join(dir, "beekeeper-broken")
	for path, raw := range map[string][]byte{goodPath: good, brokenPath: broken} {
		if err := os.WriteFile(path, raw, 0o700); err != nil { //nolint:gosec // executable candidate fixture
			t.Fatal(err)
		}
	}

	goodEv, err := StageCanary(ctx, CanaryConfig{
		Candidate: goodPath,
		Recorded:  true,
		Run:       "canary-good",
	})
	if err != nil {
		t.Fatalf("StageCanary(good) failed: %v", err)
	}
	if goodEv.Status != update.CanaryStatusSuccess || goodEv.Version != goodRelease || goodEv.SHA256 != hexSum(good) || goodEv.MergeCommit == "" {
		t.Fatalf("unexpected good canary evidence: %+v", goodEv)
	}

	brokenEv, err := StageCanary(ctx, CanaryConfig{
		Candidate: brokenPath,
		Recorded:  true,
		Run:       "canary-broken-parser",
	})
	if err == nil {
		t.Fatalf("StageCanary(broken) unexpectedly succeeded: %+v", brokenEv)
	}
	if brokenEv.Status != update.CanaryStatusFailed || brokenEv.MergeCommit != "" || brokenEv.SHA256 != hexSum(broken) || brokenEv.Reason == "" {
		t.Fatalf("unexpected broken canary evidence: %+v", brokenEv)
	}

	noCheckoutEv, err := StageCanary(ctx, CanaryConfig{
		Candidate: goodPath,
		Recorded:  true,
		Run:       "canary-missing-merge-validation",
		Scenario:  "no-checkout",
	})
	if err == nil || noCheckoutEv.Status != update.CanaryStatusFailed || noCheckoutEv.MergeCommit != "" {
		t.Fatalf("StageCanary(no-checkout) = %+v, %v; want failed evidence", noCheckoutEv, err)
	}

	// Exercise the actual coordinator (update.Run) and execution-host
	// (update.InstallVersion and update.InstallRelease) installer routes against
	// the StageCanary output and verify installed binary bytes on disk.
	verifyRoutes := func(runningVer, targetVer string, candBytes []byte, ev update.CanaryEvidence, wantInstall bool) {
		t.Helper()
		stateHome := t.TempDir()
		t.Setenv("XDG_STATE_HOME", stateHome)
		t.Setenv("GH_TOKEN", "test-update-token")

		exeDir := t.TempDir()
		exePath := filepath.Join(exeDir, "beekeeper")
		initialBytes := []byte("initial-installed-binary-" + runningVer)
		if err := os.WriteFile(exePath, initialBytes, 0o700); err != nil { //nolint:gosec // installed binary target
			t.Fatal(err)
		}
		assetName := fmt.Sprintf("beekeeper-%s-%s", runtime.GOOS, runtime.GOARCH)
		evJSON, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		const (
			bundleExt    = ".bundle"
			canaryExt    = ".canary.json"
			signedPrefix = "signed:"
		)
		restore := update.SetUpdateTargetForTest(exePath, runningVer, func(name string, _, bundle []byte) error {
			if string(bundle) != signedPrefix+name {
				return fmt.Errorf("invalid bundle for %s", name)
			}
			return nil
		})
		defer restore()

		dlBase := "https://github.com/" + update.Repository + "/releases/download/" + targetVer + "/"
		assets := []update.Asset{
			{ID: 1, Name: assetName, Size: len(candBytes), URL: dlBase + assetName},
			{ID: 2, Name: assetName + bundleExt, Size: len(signedPrefix + assetName), URL: dlBase + assetName + bundleExt},
			{ID: 3, Name: assetName + canaryExt, Size: len(evJSON), URL: dlBase + assetName + canaryExt},
			{ID: 4, Name: assetName + canaryExt + bundleExt, Size: len(signedPrefix + assetName + canaryExt), URL: dlBase + assetName + canaryExt + bundleExt},
		}
		assetBodies := map[int64][]byte{
			1: candBytes,
			2: []byte(signedPrefix + assetName),
			3: evJSON,
			4: []byte(signedPrefix + assetName + canaryExt),
		}
		relPayload, err := json.Marshal(map[string]any{
			"id":           1,
			"tag_name":     targetVer,
			"prerelease":   true,
			"published_at": time.Now().UTC().Format(time.RFC3339),
			"html_url":     "https://github.com/" + update.Repository + "/releases/tag/" + targetVer,
			"assets": []map[string]any{
				{"id": 1, "name": assets[0].Name, "size": assets[0].Size, "browser_download_url": assets[0].URL},
				{"id": 2, "name": assets[1].Name, "size": assets[1].Size, "browser_download_url": assets[1].URL},
				{"id": 3, "name": assets[2].Name, "size": assets[2].Size, "browser_download_url": assets[2].URL},
				{"id": 4, "name": assets[3].Name, "size": assets[3].Size, "browser_download_url": assets[3].URL},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		prevTransport := http.DefaultTransport
		defer func() { http.DefaultTransport = prevTransport }()
		http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reply := func(code int, body []byte) (*http.Response, error) {
				return &http.Response{
					StatusCode: code,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(string(body))),
					Request:    r,
				}, nil
			}
			switch {
			case strings.Contains(r.URL.Path, "/git/matching-refs/tags"):
				return reply(http.StatusOK, []byte(`[{"ref":"refs/tags/`+runningVer+`","object":{"sha":"111"}},{"ref":"refs/tags/`+targetVer+`","object":{"sha":"222"}}]`))
			case strings.Contains(r.URL.Path, "/compare/"):
				return reply(http.StatusOK, []byte(`{"status":"ahead"}`))
			case strings.HasSuffix(r.URL.Path, "/releases/tags/"+targetVer):
				return reply(http.StatusOK, relPayload)
			case strings.Contains(r.URL.Path, "/releases/assets/"):
				for id, b := range assetBodies {
					if strings.HasSuffix(r.URL.Path, fmt.Sprintf("/releases/assets/%d", id)) {
						return reply(http.StatusOK, b)
					}
				}
			}
			for i, a := range assets {
				if r.URL.String() == a.URL {
					return reply(http.StatusOK, assetBodies[int64(i+1)])
				}
			}
			return reply(http.StatusNotFound, []byte(`{}`))
		})

		routes := []struct {
			name string
			fn   func() (update.Result, error)
		}{
			{"coordinator_run", func() (update.Result, error) { return update.Run(ctx, io.Discard, false) }},
			{"host_pinned", func() (update.Result, error) { return update.InstallVersion(ctx, io.Discard, targetVer) }},
			{"host_given", func() (update.Result, error) {
				return update.InstallRelease(ctx, io.Discard, update.Release{Tag: targetVer, ID: 1, Prerelease: true, Assets: assets})
			}},
		}
		for _, rt := range routes {
			_ = os.RemoveAll(exePath + ".update")
			if err := os.WriteFile(exePath, initialBytes, 0o700); err != nil { //nolint:gosec // reset target before each route
				t.Fatal(err)
			}
			res, rerr := rt.fn()
			installedBytes, err := os.ReadFile(exePath) //nolint:gosec // verify installed target bytes
			if err != nil {
				t.Fatal(err)
			}
			if !wantInstall {
				if rerr == nil || res.Updated {
					t.Fatalf("route %s unexpectedly updated with evidence %+v: res=%+v", rt.name, ev, res)
				}
				if hexSum(installedBytes) != hexSum(initialBytes) {
					t.Fatalf("route %s modified installed executable bytes on refusal", rt.name)
				}
			} else {
				if rerr != nil || !res.Updated {
					t.Fatalf("route %s failed to install eligible candidate: res=%+v err=%v", rt.name, res, rerr)
				}
				if hexSum(installedBytes) != hexSum(candBytes) {
					t.Fatalf("route %s did not install expected candidate bytes", rt.name)
				}
			}
		}
		if ev.Status == update.CanaryStatusFailed {
			qPath := filepath.Join(stateHome, "beekeeper", "canary-quarantine", hexSum(candBytes)+".json")
			if _, qerr := os.Stat(qPath); qerr != nil {
				t.Fatalf("expected quarantine file at %s: %v", qPath, qerr)
			}
		}
	}

	// 1. Failed canary evidence blocks all three update routes and preserves installed bytes.
	verifyRoutes(goodRelease, nextRelease, broken, brokenEv, false)
	// 2. Recorded fixture success evidence cannot grant release eligibility on any route.
	verifyRoutes("v1.0.0-rc.1", goodRelease, good, goodEv, false)
	// 3. Live sandbox success evidence unlocks all three update routes.
	liveEligibleEv := goodEv
	liveEligibleEv.Run = "circleci-live-sandbox-1"
	verifyRoutes("v1.0.0-rc.1", goodRelease, good, liveEligibleEv, true)

	writeProofArtifact(t, "containment-proof.json", map[string]any{
		"good_evidence":                 liveEligibleEv,
		"recorded_fixture_evidence":     goodEv,
		"broken_parser_evidence":        brokenEv,
		"unavailable_checkout_evidence": noCheckoutEv,
		"coordinator_update_blocked":    true,
		"host_pinned_update_blocked":    true,
		"host_given_update_blocked":     true,
		"installed_sha256_preserved":    hexSum(good),
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func writeProofArtifact(t *testing.T, name string, payload any) {
	t.Helper()
	artifactRoot := strings.TrimSpace(os.Getenv("BEEKEEPER_TEST_LOOP_ARTIFACTS"))
	if artifactRoot == "" {
		return
	}
	outDir := filepath.Join(artifactRoot, "recovery-proof")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(outDir, name), raw, 0o600); err != nil {
		t.Fatal(err)
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

// This runs the actual isolated probe and verifies release discovery reaches
// the retained adapter. Ambient desk configuration and credentials stay out.
func TestRecoveryProbeReleaseWorkflowDiscovery(t *testing.T) {
	binary := os.Getenv("BEEKEEPER_TEST_LOOP_BINARY")
	if binary == "" {
		binary = buildCandidate(t, "")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), ProbeTimeout)
	defer cancel()
	p := probe{ctx: ctx, dir: t.TempDir()}
	if err := p.prepare(self); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(p.serveAPI))
	defer api.Close()
	p.env = append(p.env, "BEEKEEPER_GITHUB_API="+api.URL)
	for range 2 {
		c := exec.CommandContext(ctx, binary, "supervisor", "loop", "--once") //nolint:gosec // explicit integration candidate
		c.Env, c.Dir = p.env, p.dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("probe loop: %v: %s", err, out)
		}
	}
	if err := p.proven(); err != nil {
		t.Fatal(err)
	}
	trace, err := os.ReadFile(p.path("trace.jsonl"))
	if err != nil || !strings.Contains(string(trace), "contents/.github/workflows/zz_generated.auto_release.yaml?ref=main") {
		t.Fatalf("probe skipped release discovery: %s, %v", trace, err)
	}
}

func TestCandidateProbeIgnoresRetainedFixture(t *testing.T) {
	binary := buildCandidate(t, "")
	// The retained adapter is deliberately unusable. The legacy path must fail,
	// while the candidate-owned entrypoint must prepare and prove its own merge.
	outdated := filepath.Join(t.TempDir(), "outdated-judge")
	if err := os.WriteFile(outdated, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	if err := Probe(t.Context(), binary, outdated, t.TempDir()); err == nil {
		t.Fatal("outdated fixture unexpectedly passed")
	}
	if err := CandidateProbe(t.Context(), binary, outdated, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverOutdatedJudgeCommand(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint("broken=", broken), func(t *testing.T) {
			overlay := ""
			if broken {
				overlay = parserOverlay(t)
			}
			good, candidate := versioned(t, goodRelease, ""), versioned(t, nextRelease, overlay)
			d := newDesk(t, good, candidate)
			if err := update.Rollback(t.Context(), d.target, "isolated workflow probe failed 3 times", d.store.RecoverVersion); err != nil {
				t.Fatal(err)
			}
			retainedCandidate := filepath.Join(d.target+".update", hexSum(candidate))
			c := exec.CommandContext(t.Context(), retainedCandidate, "--config", d.config, "self-update", "--recover-outdated-judge", d.target) //nolint:gosec // verified fixture executable
			c.Env, c.Dir = d.env, d.dir
			out, err := c.CombinedOutput()
			if broken {
				if err == nil {
					t.Fatal("promoted a broken candidate")
				}
				if j := d.journal(t); !j.RolledBack() || j.JudgeRecovery != "" {
					t.Fatalf("%+v: %s", j, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("recover: %v: %s", err, out)
			}
			j := d.journal(t)
			if !j.Settled() || j.RolledBack() || j.JudgeRecovery == "" {
				t.Fatalf("%+v", j)
			}
			if d, err := update.Digest(d.target); err != nil || d != hexSum(candidate) {
				t.Fatalf("installed %s: %v", d, err)
			}
			writer, err := state.OpenVersion(filepath.Join(d.dir, "state"), nextRelease)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Update(func(st *state.State) ([]state.Event, error) {
				if len(st.Notes) != 1 || st.Notes[0].Text != "work in flight" {
					t.Fatal("lost live work")
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			var fence map[string]any
			if found, err := writer.ReadFile("recovery-writer.json", &fence); err != nil || found {
				t.Fatalf("fence remains: %v %v", found, err)
			}
		})
	}
}
